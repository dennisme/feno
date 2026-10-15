// Package suggest drafts a detector from an SLI somebody already wrote.
//
// It is the cheapest useful command in the tool for the reason spec.md Part
// 6 gives: a team keeping SLOs in git has already declared where the metrics
// live and which query matters, so the first detector costs them a review
// rather than an authoring session. Part 8.2 calls the fields in between the
// onboarding cliff, and they are the ones nobody can fill in from the
// document alone: whether the series has a daily shape, whether a spike
// widens the band enough to hide the next one, and whether the series can be
// banded at all.
//
// # Three stages, and why they are separable
//
// [Measure] reads history once and reduces it to a [Profile]. [Decide] is a
// pure function from that profile to a [Decision]. [Draft] renders the
// decision as the YAML document [spec.Load] reads back.
//
// The split is what makes the decisions testable at all. Every threshold
// here is a judgement with a cost either way, and the only way to argue
// about one is to feed a profile to [Decide] and read what it concluded. A
// version that decided while it read would be arguable only against a
// running Prometheus.
//
// It also keeps the evidence attached to the conclusion. Every field
// [Decide] sets carries the statistic it was set from, and [Draft] writes
// those lines into the document's header. A drafted number with nothing
// behind it is a number a reviewer has to take on trust, which is the thing
// this tool exists not to ask for.
//
// # Nothing here calls a model
//
// spec.md Part 6 decision 4 and AGENTS.md both say the generation path may
// not, and this is the authoring path that Part 8.2 names as the best fit
// for a typed-decision model. The heuristics here are the deterministic
// fallback that decision requires to exist and to be the default: an
// autocorrelation for seasonality and the query's own metric names for
// class. A model adapter, if one is ever built, is a second implementation
// of [Decide] in its own module, reading the same [Profile].
//
// # What a draft deliberately leaves out
//
// A field this package cannot measure is left absent so that internal/spec's
// own default applies, rather than written out as though somebody chose it.
// `window` and `minCoverage` are both that. `minWidth` is as well, and for a
// stronger reason: it is a floor in the series' own units, so a value for it
// is an opinion about how large a change is worth paging for, and no
// statistic here holds that opinion.
//
// Strategy, sensitivity and class are written because a detector without
// them does not validate. Of those, only sensitivity is unmeasured, and the
// header says so.
package suggest

import (
	"errors"
	"time"
)

// Profile is one query's history, reduced to the statistics every decision
// below is made from.
//
// It holds no samples. What a decision needs is a handful of numbers per
// series, and keeping the points would mean a month of history for every
// SLI in a directory held in memory at once for no reader.
type Profile struct {
	// Query is the PromQL the profile was read from, as the SLI wrote it.
	Query string

	// From, To and Step are the range that was read, so that a header can
	// say what the numbers describe and a reader can tell a conclusion
	// drawn from three weeks from one drawn from an afternoon.
	From, To time.Time
	Step     time.Duration

	// Instants is how many evaluation instants the range holds at Step, and
	// is what each series' coverage is a fraction of.
	Instants int

	// Series is one entry per label set the query returned, in the order the
	// reader returned them.
	Series []SeriesProfile
}

// SeriesProfile is the statistics of one label set.
type SeriesProfile struct {
	// Key is [prometheus.SeriesKey] of the label set, which is the identity
	// the rest of the tool knows a series by.
	Key string

	// Samples is how many finite samples the range held, and NonFinite is
	// how many of the returned samples were a NaN or an infinity. The two
	// are counted apart because they are different findings: missing
	// samples make a band cold and a NaN makes it undefined, and only the
	// first is fixed by reading more history.
	Samples   int
	NonFinite int

	// Median is the centre the robust strategy takes.
	Median float64

	// StdDev is the scale the adaptive strategy takes, and RobustScale is
	// the interquartile range rescaled to be comparable with it, which is
	// what internal/detect does so that a sensitivity means the same width
	// either way. The ratio between them is what [Decide] reads: on a
	// roughly normal series they agree, and a series where the standard
	// deviation is far the larger is one where a few spikes are setting the
	// band's width.
	StdDev      float64
	RobustScale float64

	// Autocorrelation is the correlation between the series and itself at
	// each lag that could be measured, keyed by the lag. A lag the range was
	// too short to hold a full period of is absent rather than zero, since
	// "no daily shape" and "could not look for one" are different answers.
	Autocorrelation map[time.Duration]float64
}

// Coverage is the fraction of the range's instants that held a finite
// sample. It is the same fraction internal/spec's minCoverage is in, so a
// series under [spec.DefaultMinCoverage] here is one whose bands would be
// cold at the coverage a detector defaults to.
func (s SeriesProfile) Coverage(instants int) float64 {
	if instants <= 0 {
		return 0
	}
	return float64(s.Samples) / float64(instants)
}

var (
	// ErrUnfit reports a series no band suits. It is the outcome spec.md
	// Part 8.2 names alongside the fields: too sparse or too spiky to band
	// at all. A draft nobody can backtest is worse than no draft, so this is
	// a refusal with the measurement behind it rather than a document with a
	// disclaimer.
	ErrUnfit = errors.New("unfit for a band")

	// ErrNoSeries reports a query that returned nothing over the range. It
	// is separate from [ErrUnfit] because the series is not the problem: the
	// query, the range or the datasource is, and all three are things the
	// person running the command can change.
	ErrNoSeries = errors.New("query returned no series")
)
