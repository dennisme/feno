package backtest

import (
	"errors"
	"fmt"
	"time"

	"github.com/dennisme/feno/internal/episode"
	"github.com/dennisme/feno/internal/prometheus"
	"github.com/dennisme/feno/internal/rules"
)

var (
	// ErrBurnRead reports a detector and a burn signal that do not go
	// together: one linked to an SLO with nothing read for it, or one linked
	// to none handed a read anyway. Either is a caller that has lost track of
	// which detector it is analysing, and the report would be about two
	// different detectors.
	ErrBurnRead = errors.New("the burn signal read does not match the detector's link")

	// ErrBurnSeries reports a burn query that returned more than one label
	// set. An SLO is one objective over one indicator, so a query returning
	// three series means the SLI query is not aggregated, and taking any
	// series that was burning would intersect the episodes with a signal
	// nobody declared.
	ErrBurnSeries = errors.New("the burn query returned more than one series")

	// ErrBurnGrid reports a burn sample that is not on the grid the detector
	// was evaluated on. Two timelines offset from each other intersect in
	// nothing, which reads as a detector that never coincided with an
	// incident, so it is refused rather than reported.
	ErrBurnGrid = errors.New("a burn sample is not on the window's grid")
)

// BurnCounts is how many of the reported window's instants the burn signal
// answered, and how many of those were burning.
//
// Absent and Undefined are kept apart from Evaluated for the same reason
// [Counts] keeps them apart for a detector: an instant whose SLI was not
// being scraped and one whose ratio divided by zero are both instants where
// nothing is known about the budget, and counting either as not burning would
// make every detector look less useful than it is.
type BurnCounts struct {
	Evaluated int
	Absent    int
	Undefined int
	Burning   int
}

// Budget is the error budget half of a report: the signal that was read for
// the SLO a detector links to, and what it said.
//
// It is nil on a report for a detector that links to no SLO. That is a
// different thing from an overlap of zero, and the report prints a different
// sentence for each: one is a detector nobody has tied to an objective, the
// other is a detector whose pages never landed on one.
type Budget struct {
	rules.Burn

	Counts BurnCounts
}

// Measured reports whether the burn signal answered at all. A query that
// returned nothing, or nothing computable, says nothing about the detector,
// so the overlap it implies is not computed rather than zero.
func (b *Budget) Measured() bool { return b.Counts.Evaluated > 0 }

// burnTimeline turns the burn read into the samples the episodes are
// intersected with, on the reported window's own grid.
//
// The timeline is built by instant rather than by sample, so it carries one
// entry per reported instant whether or not the read had a value there, and
// every entry is on the grid by construction. That is what makes the
// intersection in internal/episode an arithmetic question rather than a
// question about two servers' idea of a timestamp.
func burnTimeline(w Window, burn rules.Burn, result *prometheus.Result) ([]episode.Sample, BurnCounts, error) {
	if len(result.Series) > 1 {
		return nil, BurnCounts{}, fmt.Errorf("SLO %q: %d series: %w",
			burn.SLO, len(result.Series), ErrBurnSeries)
	}

	instants := w.Instants()
	counts := BurnCounts{Absent: instants}
	burning := make([]bool, instants)

	if len(result.Series) == 1 {
		for _, point := range result.Series[0].Points {
			offset := point.At.Sub(w.Start)
			if offset%w.Step != 0 {
				return nil, BurnCounts{}, fmt.Errorf("SLO %q: %s is not a whole %v from %s: %w",
					burn.SLO, instant(point.At), w.Step, instant(w.Start), ErrBurnGrid)
			}
			index, inside := w.instant(point.At)
			if !inside {
				// The warm-up the detector needed holds no episode, so a
				// sample before the window is not an error and not a verdict.
				continue
			}
			on, known := burn.At(point.Value)
			switch {
			case !known:
				counts.Undefined++
			case on:
				counts.Burning++
				burning[index] = true
			}
			if known {
				counts.Evaluated++
			}
			counts.Absent--
		}
	}

	timeline := make([]episode.Sample, instants)
	for i := range timeline {
		timeline[i] = episode.Sample{
			At:        w.Start.Add(time.Duration(i) * w.Step),
			Breaching: burning[i],
		}
	}
	return timeline, counts, nil
}

// TouchedEpisodes is how many paging episodes coincided with the budget
// burning, and how many there were. It is the number the report leads with,
// because an episode that never landed on a burning minute is the one a
// reader deletes.
func (r *Report) TouchedEpisodes() (touched, episodes int) {
	for _, series := range r.Series {
		touched += series.Touched()
		episodes += series.Summary.Firing
	}
	return touched, episodes
}

// CoincidentTime is the wall clock time during which somebody was paged and
// the budget was burning, counting two instances firing in the same hour
// once, the way [Report.PagedTime] does.
func (r *Report) CoincidentTime() time.Duration {
	return time.Duration(r.CoincidentInstants) * r.Window.Step
}

// CoincidentRatio is the coincident time as a fraction of the time somebody
// was paged.
//
// The denominator is the paged time and not the reported window, which is the
// one ratio in this report that does not divide by the window. The question
// behind it is what fraction of the pages landed on something real, and a
// window's worth of quiet hours has no bearing on that.
func (r *Report) CoincidentRatio() float64 {
	paged := r.PagedTime()
	if paged <= 0 {
		return 0
	}
	return float64(r.CoincidentTime()) / float64(paged)
}
