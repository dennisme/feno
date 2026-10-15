package suggest

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dennisme/feno/internal/detect"
	"github.com/dennisme/feno/internal/spec"
)

// Thresholds every decision below turns on. Each is a judgement, so each one
// says which way it is wrong in both directions.
const (
	// spikeRatio is how far the standard deviation may sit above the
	// interquartile scale before the robust strategy is drafted.
	//
	// The two are rescaled to be comparable, so on a roughly normal series
	// they agree and the ratio is near one. A ratio of one and a half is a
	// series where a minority of samples is contributing half again as much
	// width as the bulk of it, which is the failure the robust strategy
	// exists for. Lower and ordinary noise drafts `robust`, which costs a
	// seasonal band the ability to pool more than one window at all: see the
	// refusal internal/rules makes of a pooled median. Higher and a series
	// whose daily spike is already hiding the next one drafts `adaptive`.
	spikeRatio = 1.5

	// seasonalCorrelation is the correlation at a lag that drafts that lag's
	// seasonality.
	//
	// A half is where a shape is strong enough to be worth six recording
	// rules, which is what a seasonal offset costs in internal/rules. Lower
	// and a weak daily drift triples the band's history requirement and the
	// rules emitted for it. Higher and a real but noisy daily cycle is
	// banded against the last hour, so every morning ramp is a breach.
	seasonalCorrelation = 0.5

	// maxNonFinite is the fraction of returned samples that may be a NaN or
	// an infinity before the series is refused. Every window holding one
	// makes its band undefined, and a detector whose bands are mostly
	// undefined is one a backtest reports nothing about. A handful is
	// ordinary: a counter reset or a division by a zero total produces one.
	maxNonFinite = 0.01
)

// Decision is the fields a draft carries, with the evidence behind them.
//
// Only what internal/spec needs to validate a detector is here. A field this
// package leaves to internal/spec's own default is absent from this struct
// rather than set to a zero value, so that nothing downstream has to tell
// "not decided" apart from "decided to be zero".
type Decision struct {
	Strategy    detect.Strategy
	Sensitivity detect.Sensitivity
	Seasonality []spec.Seasonality
	Class       spec.Class

	// Evidence is one line per decision, naming the statistic it was made
	// from and the threshold it was compared against. [Draft] writes these
	// into the document's header, which is the whole reason a reviewer can
	// argue with a draft rather than only accept or delete it.
	Evidence []string
}

// Decide is the draft's fields, from the profile alone.
//
// # Why a majority rather than every series
//
// One query returns a label set per instance, and one detector bands all of
// them with one specification. So a decision has to be a single answer over
// a set of series that disagree, and the choice here is the median series
// for a statistic and a majority for a yes or no.
//
// The consequence worth stating: a minority of series that are too sparse or
// too spiky to band is drafted over anyway, and internal/detect reports
// their instants Cold rather than quiet. That is the right trade, because
// the alternative refuses a whole service's detector over one instance that
// was rebooting while the profile was read. A majority unfit is refused,
// because at that point the query is the problem.
//
// Statistics are taken over the fit series only. Including the unfit ones
// would let the instance that was down set the band width for the ones that
// were not.
func Decide(p Profile) (Decision, error) {
	if len(p.Series) == 0 {
		return Decision{}, fmt.Errorf("%s: %w",
			p.Query, ErrNoSeries)
	}

	var (
		fit     []SeriesProfile
		reasons []string
	)
	for _, series := range p.Series {
		if reason := unfit(series, p.Instants); reason != "" {
			reasons = append(reasons, fmt.Sprintf("%s %s", series.Key, reason))
			continue
		}
		fit = append(fit, series)
	}
	if len(reasons)*2 > len(p.Series) {
		return Decision{}, fmt.Errorf("%d of %d series %w: %s",
			len(reasons), len(p.Series), ErrUnfit, strings.Join(reasons, "; "))
	}

	decision := Decision{
		Class:       class(p.Query),
		Sensitivity: detect.Balanced,
	}
	decision.Evidence = append(decision.Evidence, classEvidence(decision.Class))

	strategy, evidence := strategy(fit)
	decision.Strategy = strategy
	decision.Evidence = append(decision.Evidence, evidence)

	seasonality, evidence := seasonality(fit)
	decision.Seasonality = seasonality
	decision.Evidence = append(decision.Evidence, evidence)

	decision.Evidence = append(decision.Evidence,
		fmt.Sprintf("sensitivity %s: not profiled. How many units of scale is a policy about who gets paged, "+
			"not a property of the series", detect.Balanced),
		coverageEvidence(fit, p.Instants),
	)
	if len(reasons) > 0 {
		decision.Evidence = append(decision.Evidence,
			fmt.Sprintf("%d of %d series are unfit and were left out of the statistics above: %s",
				len(reasons), len(p.Series), strings.Join(reasons, "; ")))
	}
	return decision, nil
}

// unfit is why no band suits this series, or empty when one does.
//
// The order is the order the reasons are worth hearing in: a series nobody
// has samples for cannot be judged on its shape, and a flat series is a
// different finding from a spiky one even though both are about the scale.
func unfit(s SeriesProfile, instants int) string {
	if coverage := s.Coverage(instants); coverage < spec.DefaultMinCoverage {
		return fmt.Sprintf("is too sparse to band: %s of %d instants held a sample, under the %s a band needs",
			percent(coverage), instants, percent(spec.DefaultMinCoverage))
	}
	if total := s.Samples + s.NonFinite; total > 0 {
		if fraction := float64(s.NonFinite) / float64(total); fraction > maxNonFinite {
			return fmt.Sprintf("holds too many non-finite samples to band: %d of %d are a NaN or an infinity, "+
				"and a window holding one has an undefined band", s.NonFinite, total)
		}
	}
	if s.StdDev == 0 && s.RobustScale == 0 {
		return "is constant over the whole range: a band of no width breaches on the first sample that moves"
	}
	// A band is its centre plus a margin times the scale, so a scale at the
	// level puts the lower edge at or below zero for a series that cannot go
	// negative. Half the band is then unreachable and the other half is
	// wider than the series itself.
	if s.RobustScale >= math.Abs(s.Median) {
		return fmt.Sprintf("is too spiky to band: its scale of %s is at or above its own level of %s, "+
			"so the band would be wider than the series", number(s.RobustScale), number(s.Median))
	}
	return ""
}

// classNames is what a query has to mention for each class, in the order the
// classes are tested.
//
// Order is the whole content of this heuristic. A latency query is full of
// request words, and an error query is full of totals, so the specific names
// are tested before the general ones and [spec.ClassRequests] is what is
// left. The names are matched against the query text the SLI author wrote,
// which is the only evidence available offline and is wrong often enough
// that the class is in the draft's header for a person to correct.
var classNames = []struct {
	class spec.Class
	names []string
}{
	{spec.ClassLatency, []string{"latency", "duration", "_seconds_bucket", "histogram_quantile", "_seconds_sum", "apdex"}},
	{spec.ClassErrors, []string{"error", "_fail", "failure", "exception", "rejected", "timeout"}},
	{spec.ClassSaturation, []string{"memory", "cpu", "saturation", "utilization", "utilisation", "_usage",
		"queue", "backlog", "inflight", "in_flight", "_connections", "_pool_", "disk", "_bytes"}},
	{spec.ClassRequests, []string{"request", "_total", "rate(", "throughput", "qps"}},
}

// class is what kind of signal the query reads, from the names in it.
func class(query string) spec.Class {
	lowered := strings.ToLower(query)
	for _, candidate := range classNames {
		for _, name := range candidate.names {
			if strings.Contains(lowered, name) {
				return candidate.class
			}
		}
	}
	return spec.ClassRequests
}

func classEvidence(c spec.Class) string {
	return fmt.Sprintf("class %s: read from the names in the query, which is a guess and not a measurement. "+
		"Nothing in the band reads it, so correcting it costs nothing but is worth doing", c)
}

// strategy is which centre and scale the band takes, from how far the
// median series' standard deviation sits above its interquartile scale.
func strategy(fit []SeriesProfile) (detect.Strategy, string) {
	ratios := make([]float64, 0, len(fit))
	for _, series := range fit {
		ratios = append(ratios, scaleRatio(series))
	}
	ratio := median(ratios)

	if math.IsInf(ratio, 1) {
		return detect.Robust, fmt.Sprintf("strategy %s: the median series has an interquartile scale of zero "+
			"against a standard deviation that is not, so a handful of samples set the whole band width",
			detect.Robust)
	}
	if ratio >= spikeRatio {
		return detect.Robust, fmt.Sprintf("strategy %s: the median series' standard deviation is %s times its "+
			"interquartile scale, at or above the %s that says spikes are setting the width",
			detect.Robust, number(ratio), number(spikeRatio))
	}
	return detect.Adaptive, fmt.Sprintf("strategy %s: the median series' standard deviation is %s times its "+
		"interquartile scale, under the %s that would say spikes are setting the width",
		detect.Adaptive, number(ratio), number(spikeRatio))
}

// scaleRatio is the standard deviation over the interquartile scale, which is
// one on a normal series because internal/detect rescales the second to be
// comparable with the first.
func scaleRatio(s SeriesProfile) float64 {
	if s.RobustScale == 0 {
		return math.Inf(1)
	}
	return s.StdDev / s.RobustScale
}

// lags is the seasonality a profile is asked about and the lag each one is
// measured at, in the order internal/spec generates offsets in so that the
// drafted list and the band agree.
var lags = []struct {
	name spec.Seasonality
	lag  time.Duration
}{
	{spec.Daily, 24 * time.Hour},
	{spec.Weekly, 7 * 24 * time.Hour},
}

// seasonality is which periods a majority of the fit series repeat over.
//
// A lag no series could be measured at is reported as unmeasured rather than
// as absent. The range read decides which lags are measurable at all, and
// "this profile could not look for a weekly shape" is a different sentence
// from "there is no weekly shape", with a different fix.
func seasonality(fit []SeriesProfile) ([]spec.Seasonality, string) {
	var (
		out   []spec.Seasonality
		notes []string
	)
	for _, candidate := range lags {
		var measured, exceeding int
		var correlations []float64
		for _, series := range fit {
			correlation, ok := series.Autocorrelation[candidate.lag]
			if !ok {
				continue
			}
			measured++
			correlations = append(correlations, correlation)
			if correlation >= seasonalCorrelation {
				exceeding++
			}
		}
		if measured == 0 {
			notes = append(notes, fmt.Sprintf("%s unmeasured, the range read was shorter than two of its periods",
				candidate.name))
			continue
		}
		correlation := median(correlations)
		if exceeding*2 > measured {
			out = append(out, candidate.name)
			notes = append(notes, fmt.Sprintf("%s at %s, over the %s threshold",
				candidate.name, number2(correlation), number2(seasonalCorrelation)))
			continue
		}
		notes = append(notes, fmt.Sprintf("no %s shape, %s against the %s threshold",
			candidate.name, number2(correlation), number2(seasonalCorrelation)))
	}

	drafted := "none"
	if len(out) > 0 {
		names := make([]string, 0, len(out))
		for _, name := range out {
			names = append(names, string(name))
		}
		drafted = strings.Join(names, ", ")
	}
	return out, fmt.Sprintf("seasonality %s: autocorrelation of the median series, %s",
		drafted, strings.Join(notes, "; "))
}

// coverageEvidence says how full the windows a band will be computed over
// were, which is what says whether the profile measured a series or an
// outage.
func coverageEvidence(fit []SeriesProfile, instants int) string {
	coverages := make([]float64, 0, len(fit))
	for _, series := range fit {
		coverages = append(coverages, series.Coverage(instants))
	}
	return fmt.Sprintf("coverage %s: the median of %d series over %d instants, measured at the profile's step "+
		"and not at the step a ruler will evaluate at", percent(median(coverages)), len(fit), instants)
}

// median is the middle of the values, averaging the two middles of an even
// count. The input is copied because a caller's slice order is its own.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// number is a statistic as a header reads it: enough digits to argue with
// and not enough to pretend the measurement was that precise.
func number(value float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(value, 'f', 1, 64), ".0")
}

// number2 is a correlation, which lives between zero and one and needs the
// second digit to be worth printing at all.
func number2(value float64) string {
	return strconv.FormatFloat(value, 'f', 2, 64)
}

// percent is a fraction as a header reads it.
func percent(fraction float64) string {
	return number(fraction*100) + "%"
}
