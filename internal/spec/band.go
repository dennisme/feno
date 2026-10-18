package spec

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/dennisme/feno/internal/detect"
)

// Defaults for the fields an author can leave out.
const (
	// DefaultWindow is how much history one band is computed over when the
	// document does not say.
	//
	// An hour is the shortest window that holds enough samples to estimate a
	// scale at every step a Prometheus deployment actually runs: at a 15s
	// scrape it is 240 samples and at a 1m evaluation interval it is 60,
	// both far above the two samples a standard deviation needs. It is also
	// short enough that a shift in level over an hour is still outside the
	// band rather than absorbed into its centre, which is the failure mode
	// of a long window and the reason a day is not the default.
	DefaultWindow = time.Hour

	// DefaultMinCoverage is the fraction of a full window that must hold
	// samples before a band is computed.
	//
	// Three quarters is a judgement, not a result, so it is worth saying
	// which way each direction is wrong. Too high and an ordinary scrape gap
	// or a rolling restart turns instants Cold, and a backtest reports
	// warm-up where the series was merely thin. Too low and a band computed
	// from a handful of samples either side of an outage is a confident
	// number taken from almost nothing, reported as not breaching. The
	// second failure is the one that makes a backtest lie, so the default
	// sits nearer the strict end, and an author who knows their series is
	// sparse lowers it.
	DefaultMinCoverage = 0.75
)

// seasonalPeriods is how many periods back a seasonal band pools history
// from.
//
// Three, for two reasons that both have a cost on either side. A pooled set
// drawn from two periods gives a median that is just the midpoint of two
// windows, so the robust strategy stops being robust; three is the smallest
// count where an unusual period can be outvoted. Going further is not free:
// PromQL reaches a seasonal offset by chaining `offset`, which is a
// recording rule per offset, so daily and weekly together already cost six
// rules against the six to ten the whole band is budgeted at in spec.md Part
// 4. Three periods also set how much history a backtest has to read, since
// [detect.Spec.Lookback] is the furthest offset plus the window: three weeks
// plus an hour for a weekly band.
const seasonalPeriods = 3

// periods is what each seasonality name means as a duration. It is also what
// [AnomalyDetector.Validate] checks a name against, so an unknown name is
// rejected in one place.
var periods = map[Seasonality]time.Duration{
	Daily:  24 * time.Hour,
	Weekly: 7 * 24 * time.Hour,
}

// seasonalityOrder fixes the order offsets are generated in, so that
// `[weekly, daily]` and `[daily, weekly]` produce the same band and the same
// rule group. Reading the authored order instead would make a diff that only
// reorders a list change the generated output.
var seasonalityOrder = []Seasonality{Daily, Weekly}

var (
	// ErrStep reports a step that cannot be sampled at.
	ErrStep = errors.New("step must be positive")

	// ErrCoverage reports a window that holds too few samples at the given
	// step for a band to be computable at all.
	ErrCoverage = errors.New("window holds too few samples at this step")
)

// Band is the detector as internal/detect reads it.
//
// # Why this lives here and imports internal/detect
//
// internal/detect is a leaf: it imports nothing of ours and nothing that
// knows a query language, which is what lets its band math be the one
// implementation that internal/rules compiles and the backtest evaluates.
// That property only survives if the dependency runs this way. If
// internal/detect imported this package the band would depend on the
// authoring format, a new field in a YAML document would recompile the band
// tests, and the authored names would have become part of the
// representation.
//
// So there is no seam and no third package holding a conversion. This
// package sits in the same layer as internal/rules and internal/backtest,
// all three reading one representation from below, and the detector turns
// into a [detect.Spec] through a method on the detector.
//
// # Why this takes a step
//
// [detect.Spec.MinSamples] is a count of samples and deliberately has no
// default, because internal/detect does not know the step the samples were
// read at. Neither does a document: the step is the backtest's query
// resolution or the ruler's evaluation interval, it differs between those
// two for the same detector, and an author cannot be asked to track it.
//
// What an author can say is how much of a window may be missing, which is
// Detection.MinCoverage and is a fraction. The count is that fraction of
// what one full window holds, so the coverage policy is written once in a
// document and the step arrives from whoever is about to evaluate or emit.
// The floor is two samples, since a scale needs two and [detect.Spec]
// rejects fewer.
//
// One window and not the pool, because [detect.Spec.MinSamples] is a floor
// each offset window clears on its own. A seasonal band pooling three
// windows therefore holds three times this count when it reports, and a
// band whose third window is empty reports nothing, which is what the rule
// does.
func (d AnomalyDetector) Band(step time.Duration) (detect.Spec, error) {
	if step <= 0 {
		return detect.Spec{}, fmt.Errorf("step %v: %w", step, ErrStep)
	}
	if err := d.Validate(); err != nil {
		return detect.Spec{}, err
	}

	detection := d.Spec.Detection
	window := DefaultWindow
	if detection.Window.String() != "" {
		window = detection.Window.Duration()
	}
	offsets := seasonalOffsets(detection.Seasonality)

	// A range selector is left-open and right-closed, so a window holds
	// exactly as many samples as the step divides into it. See the window
	// section of internal/detect's package comment.
	perWindow := int(window / step)
	if perWindow < 2 {
		return detect.Spec{}, fmt.Errorf("window %v at step %v holds %d samples: %w",
			window, step, perWindow, ErrCoverage)
	}

	coverage := detection.MinCoverage
	if coverage == 0 {
		coverage = DefaultMinCoverage
	}

	s := detect.Spec{
		Strategy:    detection.Strategy,
		RobustScale: detection.RobustScale,
		Sensitivity: detection.Sensitivity,
		Window:      window,
		Offsets:     offsets,
		MinWidth:    detection.MinWidth,
		MinSamples:  max(int(math.Ceil(coverage*float64(perWindow))), 2),
	}

	// The robust strategy needs a scale and internal/detect rejects an empty
	// one, so one of the two has to supply a default. It is here, because
	// the reason for the choice is a property of the deployment the rule will
	// run on rather than of the band: the interquartile range needs no
	// optional server feature, where a median absolute deviation on
	// Prometheus is behind promql-experimental-functions and makes the band
	// declare detect.RequireMAD. An author who has that feature on writes
	// `robustScale: mad`.
	if s.Strategy == detect.Robust && s.RobustScale == "" {
		s.RobustScale = detect.ScaleIQR
	}

	if err := s.Validate(); err != nil {
		return detect.Spec{}, err
	}
	return s, nil
}

// seasonalOffsets turns seasonality names into how far back each window is
// taken.
//
// Each name contributes its period times one through [seasonalPeriods], and
// naming both pools the union: yesterday, the day before and the day before
// that, plus the same time on each of the last three weeks. The union is not
// the statistically clean choice and the cost is worth stating. Weekly
// offsets already hold time of day fixed, so they separate a Tuesday from a
// Sunday; the daily offsets in the same pool do not, so on a weekday band
// they pull in whatever the preceding weekend did. That widens the band,
// which is the direction that pages fewer people, and an author who wants
// day of week honoured strictly writes `seasonality: [weekly]` and means it.
//
// No seasonality returns nothing, which internal/detect reads as one window
// ending at the instant itself.
func seasonalOffsets(names []Seasonality) []time.Duration {
	var out []time.Duration
	for _, name := range seasonalityOrder {
		period, ok := periods[name]
		if !ok || !slices.Contains(names, name) {
			continue
		}
		for i := 1; i <= seasonalPeriods; i++ {
			out = append(out, time.Duration(i)*period)
		}
	}
	return out
}
