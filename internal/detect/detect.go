// Package detect holds the band: what a series is expected to do, as a
// structured representation, and one evaluator over it.
//
// There is exactly one band implementation in this tool. Every target
// compiles this representation into its own dialect and the backtest
// evaluates this representation directly, because a backtest with its own
// copy of the band math predicts its own behaviour rather than production's,
// which is worse than having no backtest at all.
//
// # An intermediate representation, not query strings
//
// Nothing here emits PromQL, SQL, or anything else, and nothing here parses
// them. A [Spec] says what the band is in terms of statistics and time;
// internal/rules turns that into a recording rule group for Prometheus, a
// refreshable materialised view plus a rule for ClickHouse, and whatever
// comes after. Writing those compilers must not require a change here. The
// test of that is simple: this package imports nothing that knows a query
// language.
//
// # The window, and why its boundary is load-bearing
//
// A band at an instant is computed over a window of earlier samples, and the
// generated rule will express that window as a range selector. Prometheus
// range selectors are left-open and right-closed: `metric[3m]` evaluated at
// 12:03 covers (12:00, 12:03], so the sample at exactly 12:00 is outside it
// and the sample at 12:03 is inside. [window] does the same.
//
// This is not a detail. A backtest that includes one extra sample at the far
// edge disagrees with production on every instant where that sample mattered,
// which is exactly the instants near a step change, which is exactly where
// anomaly detectors earn their keep. The boundary is tested directly rather
// than inferred from a band's centre, because an off-by-one window still
// produces a plausible centre.
//
// # Three answers, not two
//
// A rule either fires for a series or it does not, but the reasons it does
// not are worth keeping apart in a report. [Evaluate] returns a [Status] per
// instant:
//
//   - [Evaluated]: the band was computed and Breaching means what it says.
//   - [Absent]: there was no sample at that instant. The series was stale, or
//     the query returned nothing for it.
//   - [Cold]: the window held fewer than Spec.MinSamples samples. A band
//     taken from a quarter-filled window is a confident number derived from
//     nothing, and reporting it as "not breaching" would let a backtest
//     claim a quiet first week that was really an empty one.
//   - [Undefined]: the window held a NaN or an infinity, so the band is NaN.
//
// [Undefined] is separate from [Cold] because more history fixes a cold band
// and does not fix an undefined one. Both are separate from [Absent] because
// a caller deciding whether its query range was long enough needs to tell a
// warm-up from a gap. [Samples] drops all three, since a rule produces no
// result for any of them and internal/episode already treats absence as the
// end of a run.
//
// # NaN and the infinities
//
// The reader surfaces non-finite values because the server does, so this has
// to choose. The choice is that a non-finite sample anywhere in the window
// makes the band [Undefined], and that the value being judged is compared
// normally.
//
// That matches the server for the adaptive strategy and for MAD. Prometheus
// 3.14.0 fixed `mad_over_time` to return NaN rather than 0 when its range
// holds a NaN, and `avg_over_time` and `stddev_over_time` propagate a NaN out
// of their range already. A NaN band means the generated rule's comparison is
// against NaN, which is false, so the alert expression returns nothing for
// that series: [Undefined] and [Absent] are indistinguishable to the rule,
// and are kept apart only for the report.
//
// The value at the instant is compared with the ordinary rules, so NaN
// breaches nothing, matching a Prometheus comparison dropping a label set
// whose comparison is false, and an infinity breaches. Note that with no
// seasonal offset the instant is inside its own window, so a non-finite value
// there makes the band [Undefined] before the comparison is ever reached.
// That, too, is what the generated rule does.
//
// # Two robust scales that mean the same thing
//
// `robust` compiles to `mad_over_time` where the server has experimental
// functions enabled and to the interquartile range where it does not, and the
// sensitivity knob has to mean the same thing either way or a rule silently
// changes width when it moves between servers. Both are converted to a scale
// comparable to a standard deviation, 1.4826 times the MAD and the IQR
// divided by 1.349, which are the constants that make each equal the standard
// deviation of a normal distribution. TestScalesAgree is the check that this
// holds, and the capability model rests on it.
//
// # Seasonality and how much history a band needs
//
// Spec.Offsets are how far back the window is taken, one window per offset,
// pooled into a single sample set. No offsets means one window ending at the
// instant itself. Daily seasonality is a short window at offsets of a day,
// two days and so on; weekly seasonality is the same at multiples of a week.
//
// [Spec.Lookback] is the consequence, and the backtest has to extend its
// query range by exactly that much before its first evaluable instant. A
// 30 day window with weekly seasonality reads a 30 day window ending a week
// before the instant, so it needs 37 days of history, not 30 and not 7.
//
// # What is not tested
//
// Every test here is a pure function over numbers with expected values worked
// out by hand or from a stated formula. Nothing here has been checked against
// a running Prometheus, so the claims above about server behaviour are read
// from its source and release notes, not observed.
//
// One case is knowingly left open. Treating an infinity in the window as
// [Undefined] matches what `stddev_over_time` does with an infinite range,
// but `quantile_over_time` returns an infinity rather than NaN, so the IQR
// form of the robust strategy may diverge from this on a window holding one.
// Settling that needs an integration test against a real server, which this
// repository has nowhere to run yet, and asserting it from nothing is how a
// backtest starts lying.
package detect

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// Strategy is how the band's centre and scale are taken from the window.
type Strategy string

const (
	// Adaptive is the mean plus the standard deviation. It is the right
	// choice for a series that is roughly normal around its own level, and
	// the wrong one for a spiky series, where a single spike widens the band
	// enough to hide the next one.
	Adaptive Strategy = "adaptive"

	// Robust is the median plus a robust scale, which a spike moves far less.
	Robust Strategy = "robust"
)

// RobustScale is which robust measure of spread [Robust] uses. The two are
// interchangeable by design: a target picks whichever it can express, and
// both are converted to the same units.
type RobustScale string

const (
	// ScaleMAD is the median absolute deviation, which reads every sample's
	// distance from the median.
	ScaleMAD RobustScale = "mad"

	// ScaleIQR is the interquartile range, which reads two order statistics.
	// It is a genuinely robust scale rather than a degraded substitute, and
	// it needs no optional server feature, which is what makes it usable as
	// a default.
	ScaleIQR RobustScale = "iqr"
)

// Sensitivity is the knob an author actually turns. It sets how many units of
// scale the band extends either side of its centre.
type Sensitivity string

const (
	// Aggressive is two scale units, which on normal data leaves about 4.6%
	// of samples outside the band.
	Aggressive Sensitivity = "aggressive"

	// Balanced is three scale units, about 0.27% outside.
	Balanced Sensitivity = "balanced"

	// Conservative is four scale units, about 0.006% outside. Conservative
	// means conservative about paging somebody, so it is the widest band and
	// the fewest alerts.
	Conservative Sensitivity = "conservative"
)

// margins resolves a sensitivity to scale units.
var margins = map[Sensitivity]float64{
	Aggressive:   2,
	Balanced:     3,
	Conservative: 4,
}

// The constants that put the two robust scales in the same units.
//
// They are exported because an emitter compiles the same conversion into its
// own dialect, and a target carrying its own copy of either number is how a
// generated rule and the backtest end up with bands of different widths.
const (
	// MADToSigma scales a median absolute deviation to the standard deviation
	// it corresponds to on normally distributed data.
	MADToSigma = 1.4826

	// IQRToSigma does the same for an interquartile range, by division.
	IQRToSigma = 1.349
)

// Requirement is an optional capability a band needs from whatever will
// evaluate it.
//
// Capability is a property of the deployment and not of the dialect: the same
// specification against the same PromQL compiles differently depending on
// which experimental functions that particular server was started with.
// Nothing here probes a server and nothing here degrades silently. A band
// says what it requires, and the caller holding a target decides what to do
// about it.
//
// This is deliberately only what a band requires today. A registry of targets
// and an emitter interface belong with the emitters, which do not exist yet.
type Requirement string

const (
	// RequireMAD is a robust scale taken from the median absolute deviation.
	// On Prometheus that is `mad_over_time`, which is experimental and off
	// unless the server was started with promql-experimental-functions.
	// [ScaleIQR] is the form that requires nothing.
	RequireMAD Requirement = "median absolute deviation"

	// RequireSeasonalDecomposition is a band whose window is taken at
	// seasonal offsets rather than ending at the instant. ClickHouse has this
	// as a built-in; PromQL reaches it by chaining `offset`, which costs a
	// recording rule per offset.
	RequireSeasonalDecomposition Requirement = "seasonal decomposition"
)

// Status is why an instant does or does not carry a band. See the package
// comment for what each one means and why they are kept apart.
type Status string

const (
	Evaluated Status = "evaluated"
	Absent    Status = "absent"
	Cold      Status = "cold"
	Undefined Status = "undefined"
)

var (
	// ErrStrategy reports an unknown strategy.
	ErrStrategy = errors.New("unknown strategy")

	// ErrScale reports a robust scale that is missing, unknown, or set on a
	// strategy that does not use one.
	ErrScale = errors.New("robust scale")

	// ErrSensitivity reports an unknown sensitivity.
	ErrSensitivity = errors.New("unknown sensitivity")

	// ErrWindow reports a window that cannot hold samples.
	ErrWindow = errors.New("window must be positive")

	// ErrOffset reports an offset that is negative or repeated. A repeat is
	// rejected rather than deduplicated because it weighs the same history
	// twice, which is a mistake in whatever built the spec rather than a
	// preference to honour.
	ErrOffset = errors.New("offsets must be distinct and not negative")

	// ErrMinWidth reports a negative minimum width.
	ErrMinWidth = errors.New("minimum width must not be negative")

	// ErrMinSamples reports a missing or unusable minimum sample count.
	ErrMinSamples = errors.New("minimum samples must be at least 2")
)

// Spec is the band, as the thing that compiles and the thing that evaluates
// both read it.
type Spec struct {
	// Strategy and RobustScale decide the centre and the scale. RobustScale
	// is set only when Strategy is [Robust].
	Strategy    Strategy
	RobustScale RobustScale

	// Sensitivity sets how far the band extends either side of its centre.
	Sensitivity Sensitivity

	// Window is how much history one band is computed over.
	Window time.Duration

	// Offsets are how far back each window is taken. Empty means one window
	// ending at the instant being evaluated.
	Offsets []time.Duration

	// MinWidth is a floor on the band's full width, so a series that has been
	// flat for a week does not get a band of zero width that the next change
	// of any size at all breaches. It is in the series' own units.
	MinWidth float64

	// MinSamples is how many samples the pooled window must hold before a
	// band is worth computing. There is no default, because this package does
	// not know the step the samples were read at and a guess here is the
	// difference between a warm-up and a reported quiet period. The caller
	// has the step and the window, so the caller decides what coverage it
	// will accept.
	MinSamples int
}

// Validate reports whether the spec describes a band that can be evaluated.
func (s Spec) Validate() error {
	switch s.Strategy {
	case Adaptive:
		if s.RobustScale != "" {
			return fmt.Errorf("%q is set on strategy %q: %w", s.RobustScale, s.Strategy, ErrScale)
		}
	case Robust:
		switch s.RobustScale {
		case ScaleMAD, ScaleIQR:
		default:
			return fmt.Errorf("%q: %w", s.RobustScale, ErrScale)
		}
	default:
		return fmt.Errorf("%q: %w", s.Strategy, ErrStrategy)
	}

	if _, ok := margins[s.Sensitivity]; !ok {
		return fmt.Errorf("%q: %w", s.Sensitivity, ErrSensitivity)
	}
	if s.Window <= 0 {
		return fmt.Errorf("window %v: %w", s.Window, ErrWindow)
	}
	for i, d := range s.Offsets {
		if d < 0 {
			return fmt.Errorf("offset %v: %w", d, ErrOffset)
		}
		if slices.Contains(s.Offsets[:i], d) {
			return fmt.Errorf("offset %v appears twice: %w", d, ErrOffset)
		}
	}
	if s.MinWidth < 0 {
		return fmt.Errorf("minimum width %v: %w", s.MinWidth, ErrMinWidth)
	}
	if s.MinSamples < 2 {
		return fmt.Errorf("minimum samples %d: %w", s.MinSamples, ErrMinSamples)
	}
	return nil
}

// Lookback is how much history has to exist before the instant for a band to
// be computable at it, which is what a backtest extends its query range by.
func (s Spec) Lookback() time.Duration {
	var furthest time.Duration
	for _, d := range s.Offsets {
		furthest = max(furthest, d)
	}
	return furthest + s.Window
}

// Margin is how many units of scale the band extends either side of its
// centre, which is what [Sensitivity] resolves to.
//
// An emitter needs this number to compile the band and must not carry its
// own copy of the table: a second table is a second place to change when a
// sensitivity moves, and the two would disagree silently until somebody
// compared a generated rule against a backtest.
func (s Spec) Margin() float64 { return margins[s.Sensitivity] }

// Requires is what this band needs from the server that will evaluate it.
func (s Spec) Requires() []Requirement {
	var out []Requirement
	if s.Strategy == Robust && s.RobustScale == ScaleMAD {
		out = append(out, RequireMAD)
	}
	if len(s.Offsets) > 0 {
		out = append(out, RequireSeasonalDecomposition)
	}
	return out
}

// offsets is Offsets with the no-seasonality case spelled out, so everything
// downstream reads one window per offset and has no special case.
func (s Spec) offsets() []time.Duration {
	if len(s.Offsets) == 0 {
		return []time.Duration{0}
	}
	return s.Offsets
}

// Band is the band at one instant.
//
// Margin is carried alongside the centre and the scale so that a band is
// enough on its own to say what breaches it, which is what lets a report
// print an upper and lower edge without holding the spec that produced them.
type Band struct {
	Centre   float64
	Scale    float64
	Margin   float64
	MinWidth float64
}

// HalfWidth is the distance from the centre to either edge.
func (b Band) HalfWidth() float64 {
	h := b.Margin * b.Scale
	if floor := b.MinWidth / 2; h < floor {
		h = floor
	}
	return h
}

// Upper is the band's top edge.
func (b Band) Upper() float64 { return b.Centre + b.HalfWidth() }

// Lower is the band's bottom edge.
func (b Band) Lower() float64 { return b.Centre - b.HalfWidth() }

// Breaches reports whether a value lies outside the band. The comparison is
// strict at both edges, so a value sitting exactly on one is inside, and a
// NaN breaches nothing because every comparison against it is false.
func (b Band) Breaches(v float64) bool { return v > b.Upper() || v < b.Lower() }

// band computes the band over a pooled window.
func (s Spec) band(pool []float64) Band {
	b := Band{Margin: margins[s.Sensitivity], MinWidth: s.MinWidth}

	switch s.Strategy {
	case Adaptive:
		b.Centre = mean(pool)
		b.Scale = stddev(pool, b.Centre)

	case Robust:
		sorted := slices.Sorted(slices.Values(pool))
		b.Centre = quantile(0.5, sorted)

		switch s.RobustScale {
		case ScaleMAD:
			deviations := make([]float64, len(sorted))
			for i, v := range sorted {
				deviations[i] = math.Abs(v - b.Centre)
			}
			slices.Sort(deviations)
			b.Scale = MADToSigma * quantile(0.5, deviations)

		case ScaleIQR:
			b.Scale = (quantile(0.75, sorted) - quantile(0.25, sorted)) / IQRToSigma
		}
	}

	return b
}

func mean(vs []float64) float64 {
	var sum float64
	for _, v := range vs {
		sum += v
	}
	return sum / float64(len(vs))
}

// StdDev is the spread the adaptive strategy takes, over a set of samples
// whose mean it computes itself.
//
// It is exported for the same reason [IQRToSigma] is: a profile that picks a
// strategy by comparing this against the robust scale has to be measuring
// the same two numbers the band will be built from, and a second
// implementation of either would make that comparison about itself.
func StdDev(vs []float64) float64 { return stddev(vs, mean(vs)) }

// stddev divides by the sample count rather than one less, which is what
// `stddev_over_time` does. The difference is small on a full window and is
// not the point: the point is that the backtest and the rule use the same
// one.
func stddev(vs []float64, mean float64) float64 {
	var sum float64
	for _, v := range vs {
		d := v - mean
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(vs)))
}

// Quantile is [quantile] over a slice the caller has already sorted.
//
// Exported because the median and the interquartile range of a profiled
// series have to be taken the same way the robust band takes them. A profile
// using a different convention, such as averaging the middle pair of an even
// count, would report a scale the band will not use.
func Quantile(q float64, sorted []float64) float64 { return quantile(q, sorted) }

// quantile interpolates linearly between the two values either side of
// q*(n-1) in a sorted slice, which is how Prometheus computes
// `quantile_over_time` and the two medians inside `mad_over_time`. Picking a
// different convention here, such as averaging the middle pair of an even
// count, would move every robust band by a fraction of a sample.
func quantile(q float64, sorted []float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}

	rank := q * float64(len(sorted)-1)
	lower := int(math.Floor(rank))
	upper := int(math.Ceil(rank))
	weight := rank - float64(lower)

	return sorted[lower]*(1-weight) + sorted[upper]*weight
}
