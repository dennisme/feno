// Package backtest replays a detector over the history a datasource already
// holds and reports what it would have done.
//
// This is the command the whole tool exists for. Nothing in the open source
// tool chain answers "how often would this have paged me last month", and the
// answer has to arrive before anybody is asked to deploy anything, which is
// why this needs only read access and writes nothing.
//
// # Nothing here is band math or episode grouping
//
// internal/detect computes the band, internal/episode groups breaches into
// the episodes a person would have been paged for, and internal/prometheus
// reads the history. All three are tested on their own. What is left, and all
// that is here, is the query range, the aggregation across the label sets one
// query returns, and the report. Arithmetic over samples appearing in this
// package is the signal that one of those three is missing a field.
//
// # The range asked for is not the range reported on
//
// [detect.Spec.Lookback] is how much history has to exist before an instant
// for a band to be computable at it. So a request for the last thirty days is
// a request for a verdict on thirty days, and the read is longer than that by
// the lookback. A weekly band pools three weeks of offsets, so thirty days
// reported is fifty-one days read.
//
// The boundary goes there rather than the other way round. Reading thirty days
// and reporting on the nine that survive the warm-up would answer a question
// nobody asked, and it would answer it differently for every detector in the
// same directory, because the lookback is a property of the band. [Window]
// carries both spans and the report prints both, because the number that
// matters, firing time as a fraction of the window, is meaningless without
// knowing which window.
//
// The cost is real: fifty-one days at a one minute step is seventy-three
// thousand intervals against a server that refuses anything over eleven
// thousand, so the read is seven requests. internal/prometheus plans and
// stitches those, and this is the first caller that makes it do so.
//
// # One step, decided once
//
// The step is the query resolution, the grid every instant is matched on, the
// interval internal/episode tells an absence from a consecutive sample by,
// and what turns a detector's coverage fraction into [detect.Spec.MinSamples].
// It arrives once, in [Options], and every one of those four reads the same
// value. A backtest at a one minute step predicts a rule evaluated at one
// minute and no other, which is why the report prints it next to the band.
//
// # What a report counts when four instances breach at once
//
// One query returns many label sets. internal/detect evaluates one series and
// internal/episode groups one series, so the aggregation is here.
//
// The unit counted is the series episode, because that is the unit Prometheus
// pages on: an alerting expression produces one alert per label set, so four
// instances breaching the same minute is four pages and not one incident. The
// headline count is therefore the sum across series, and the per-series table
// says which label sets produced them, worst first.
//
// A second number is printed beside it, because the first one alone overstates
// what a person experienced: the wall clock time during which at least one
// series was firing, and the peak number firing at once. Those two together
// are the difference between a detector that pages nine times across a month
// and one that pages nine times in the same ten minutes. Neither is a regroup
// of episodes across series, which is a different question from the one
// internal/episode answers, and is where a merged-incident view would belong
// if a report ever needs one.
//
// # The denominator, and the two choices that flatter a rule
//
// internal/episode deliberately does not compute firing time as a fraction,
// because the denominator is a property of the request rather than of the
// samples. Here it is the reported window: [Window.Start] to [Window.End], the
// range the caller asked for, warm-up excluded, whatever the samples did
// inside it.
//
// Two other denominators were available and both flatter the rule:
//
//   - The span the returned samples cover. A query that returned nothing for
//     its first week would divide by twenty-three days instead of thirty, and
//     a detector would look quieter precisely because its series was missing,
//     which is the case a backtest most needs to show.
//   - The reported window times the number of series. Summed firing time over
//     series-time reads as a tiny fraction and gets smaller every time the
//     query matches another instance, while the pager gets louder. The
//     per-series ratios in the table are each over the window, so they can sum
//     past one hundred percent, and that is the honest reading: three
//     instances firing all month is three hundred percent of a month of pages.
//
// # Error budget overlap is a seam here, not a number
//
// The README promises overlap with an error budget actually burning, and it is
// the most useful number on the list: an anomaly episode that never coincided
// with a burn alert is a candidate for deletion. It is not computed in this
// slice and is not quietly dropped either. What it needs is a burn-rate query
// per SLO, which means reading the OpenSLO half of the directory that
// internal/spec already keeps in [spec.Bundle.Objects], compiling the SLO's
// objective and windows into a second query, a second range read, and an
// intersection of the firing timeline below with that query's. The second
// query is the part that is not written: an SLO's burn rate is sloth's and
// pyrra's output, not this tool's, so the choice is between generating it and
// being handed it. The report names the gap in its own output so that a reader
// cannot mistake a missing number for a zero.
//
// # What is not tested
//
// [Analyse] is a pure function over whatever the reader returned, so the
// fixtures in testdata exercise it without a server, and the report's shape is
// pinned by a golden file. [Run] is the wiring over internal/prometheus and is
// covered by a build-tagged integration test that says what it needs, because
// this repository has nowhere to run a Prometheus.
//
// The fixtures are deliberately at a one hour step with hour-scale windows.
// The arithmetic being checked is the grid, the warm-up boundary and the
// aggregation, none of which know what a sample is worth, and a fixture
// carrying a month of minutes would be forty thousand numbers nobody reviews.
package backtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dennisme/feno/internal/detect"
	"github.com/dennisme/feno/internal/episode"
	"github.com/dennisme/feno/internal/prometheus"
	"github.com/dennisme/feno/internal/rules"
	"github.com/dennisme/feno/internal/spec"
)

var (
	// ErrStep reports a step that cannot describe an evaluation interval.
	ErrStep = errors.New("step must be positive")

	// ErrRange reports an end before the start.
	ErrRange = errors.New("end must not be before start")

	// ErrWarmUp reports a window whose warm-up is shorter than the band's
	// lookback, so the first reported instants would be cold for want of
	// history that was never asked for. It is an error rather than a note
	// because the window and the band would then describe different runs.
	ErrWarmUp = errors.New("window warm-up is shorter than the band's lookback")

	// ErrSourceType reports a detector pointed at something this reader does
	// not speak.
	ErrSourceType = errors.New("not a Prometheus datasource")
)

// Window is the span a backtest reads and the span it reports on.
//
// Start and End are the first and last instants a verdict is given for, and
// are the range the caller asked for. ReadStart is earlier by the band's
// lookback, rounded up to a whole number of steps so that every instant read
// sits on the same grid as every instant reported: internal/detect matches a
// sample to an instant by exact timestamp, so a read that began half a step
// early would contribute no samples at all.
type Window struct {
	Start     time.Time
	End       time.Time
	ReadStart time.Time
	Step      time.Duration
}

// NewWindow is the window for a request from `from` to `to` at step, for a
// band needing lookback of history.
//
// End is truncated to the grid that starts at `from`, because a range query
// evaluates at its start and then every step, so a final remainder shorter
// than a step holds no instant and asking for a verdict there would invent
// one.
func NewWindow(from, to time.Time, step, lookback time.Duration) (Window, error) {
	if step <= 0 {
		return Window{}, fmt.Errorf("step %v: %w", step, ErrStep)
	}
	if to.Before(from) {
		return Window{}, fmt.Errorf("from %s, to %s: %w", from, to, ErrRange)
	}

	warmUp := lookback / step * step
	if warmUp < lookback {
		warmUp += step
	}

	return Window{
		Start:     from,
		End:       from.Add(to.Sub(from) / step * step),
		ReadStart: from.Add(-warmUp),
		Step:      step,
	}, nil
}

// Reported is the span a verdict is given for.
func (w Window) Reported() time.Duration { return w.End.Sub(w.Start) }

// Read is the span the reader is asked for, warm-up included.
func (w Window) Read() time.Duration { return w.End.Sub(w.ReadStart) }

// WarmUp is the history read before the first reported instant.
func (w Window) WarmUp() time.Duration { return w.Start.Sub(w.ReadStart) }

// Instants is how many evaluations the reported window holds, both ends
// included, which is what a range query at this step returns.
func (w Window) Instants() int { return int(w.Reported()/w.Step) + 1 }

// instant is the index of t in the reported window, and whether it lies
// inside it at all.
func (w Window) instant(t time.Time) (int, bool) {
	if t.Before(w.Start) || t.After(w.End) {
		return 0, false
	}
	return int(t.Sub(w.Start) / w.Step), true
}

// Options is what a caller asks for.
//
// From and To are the window reported on, not the window read: see the package
// comment. Step is the query resolution and the evaluation interval the
// report's numbers predict.
type Options struct {
	From time.Time
	To   time.Time
	Step time.Duration
}

// Counts is how many of a series' reported instants carried each answer.
//
// Cold and Undefined are kept apart from Absent because they are different
// problems with the same symptom of an empty report: a cold instant wants more
// history or a lower coverage fraction, an undefined one wants a query that
// does not divide by zero, and an absent one wants a series that was being
// scraped. All three are instants the generated rule would produce no result
// for, so none of them can breach.
type Counts struct {
	Evaluated int
	Absent    int
	Cold      int
	Undefined int
}

// SeriesReport is one label set's verdict.
type SeriesReport struct {
	Key      string
	Labels   map[string]string
	Counts   Counts
	Episodes []episode.Episode
	Summary  episode.Summary
}

// Report is what a person reads.
type Report struct {
	// Detector is the name as authored, and Query is what was read.
	Detector string
	Query    string

	// Window is the span reported on and the span read. Band and For are the
	// band that was evaluated and the hold duration episodes were grouped at,
	// both printed so that a report explains its own numbers.
	Window Window
	Band   detect.Spec
	For    time.Duration

	// Stats is what the read cost and what the server warned about. A warning
	// changes what the numbers mean, so it is carried rather than logged.
	Stats prometheus.Stats

	// Series is one entry per label set the query returned, ordered by firing
	// time and then by key, so a report is stable across runs.
	Series []SeriesReport

	// PagedInstants is how many reported instants had at least one series
	// firing, and Peak is the most series firing at any one instant.
	PagedInstants int
	Peak          int
	PeakAt        time.Time
}

// Analyse evaluates the band over history and groups the breaches, one series
// at a time, and aggregates the result.
//
// result is whatever the reader returned for the window's full read span. It
// is a pure function from there on, which is what makes the report testable
// against fixtures rather than against a server.
func Analyse(d spec.Detector, w Window, result *prometheus.Result) (*Report, error) {
	if got := d.SourceType(); got != rules.SourceType {
		return nil, fmt.Errorf("detector %q: %q: %w", d.Metadata.Name, got, ErrSourceType)
	}
	query, err := rules.Query(d)
	if err != nil {
		return nil, err
	}
	band, err := d.Band(w.Step)
	if err != nil {
		return nil, err
	}
	if lookback := band.Lookback(); w.WarmUp() < lookback {
		return nil, fmt.Errorf("warm-up %v, band needs %v: %w", w.WarmUp(), lookback, ErrWarmUp)
	}

	// A DurationShorthand that was never set has no unit, and Duration panics
	// on one, so the zero value is read as no hold rather than converted.
	var hold time.Duration
	if f := d.Spec.Alerting.For; f.String() != "" {
		hold = f.Duration()
	}

	report := &Report{
		Detector: d.Metadata.Name,
		Query:    query,
		Window:   w,
		Band:     band,
		For:      hold,
		Stats:    result.Stats,
	}

	// MaxGap is left at zero, which is the Prometheus behaviour: an
	// evaluation where the series is missing is indistinguishable to a rule
	// from one where it is present and not breaching, and either drops a
	// pending alert outright. The ClickHouse target will have to set it to
	// whatever clickhouse-ruler does.
	opts := episode.Options{Step: w.Step, For: hold}
	instants := detect.Range{Start: w.Start, End: w.End, Step: w.Step}

	for _, series := range result.Series {
		results, err := detect.Evaluate(band, instants, series.Points)
		if err != nil {
			return nil, fmt.Errorf("series %s: %w", series.Key, err)
		}
		episodes, err := episode.Extract(detect.Samples(results), opts)
		if err != nil {
			return nil, fmt.Errorf("series %s: %w", series.Key, err)
		}

		report.Series = append(report.Series, SeriesReport{
			Key:      series.Key,
			Labels:   series.Labels,
			Counts:   count(results),
			Episodes: episodes,
			Summary:  episode.Summarize(episodes),
		})
	}

	// Worst first, by the number the report leads with, then by key so that
	// two series with identical verdicts do not swap places between runs.
	sort.SliceStable(report.Series, func(i, j int) bool {
		a, b := report.Series[i], report.Series[j]
		if a.Summary.FiringTime != b.Summary.FiringTime {
			return a.Summary.FiringTime > b.Summary.FiringTime
		}
		return a.Key < b.Key
	})

	report.PagedInstants, report.Peak, report.PeakAt = concurrency(w, report.Series)
	return report, nil
}

// Run reads the history a detector needs and analyses it.
//
// The read is one range query over the window's full span, which the client
// splits into as many requests as the server's resolution limit demands.
func Run(ctx context.Context, client *prometheus.Client, d spec.Detector, opts Options) (*Report, error) {
	band, err := d.Band(opts.Step)
	if err != nil {
		return nil, err
	}
	window, err := NewWindow(opts.From, opts.To, opts.Step, band.Lookback())
	if err != nil {
		return nil, err
	}
	query, err := rules.Query(d)
	if err != nil {
		return nil, err
	}

	result, err := client.QueryRange(ctx, query, window.ReadStart, window.End, window.Step)
	if err != nil {
		return nil, fmt.Errorf("detector %q: %w", d.Metadata.Name, err)
	}
	return Analyse(d, window, result)
}

// count tallies the answers over one series' reported instants.
func count(results []detect.Result) Counts {
	var c Counts
	for _, r := range results {
		switch r.Status {
		case detect.Evaluated:
			c.Evaluated++
		case detect.Absent:
			c.Absent++
		case detect.Cold:
			c.Cold++
		case detect.Undefined:
			c.Undefined++
		}
	}
	return c
}

// concurrency walks every firing episode onto the window's grid and reports
// how many instants had anybody being paged, the most series firing at once,
// and when that peak was.
//
// Only the firing part of an episode is counted, from the instant it survived
// For to the one it ended at, because that is the span an alert would have
// been at Alertmanager. A flap contributes nothing: nobody was woken.
func concurrency(w Window, series []SeriesReport) (paged, peak int, peakAt time.Time) {
	firing := make([]int, w.Instants())
	for _, s := range series {
		for _, e := range s.Episodes {
			if !e.Fired() {
				continue
			}
			for t := e.FiredAt; t.Before(e.End); t = t.Add(w.Step) {
				if i, ok := w.instant(t); ok {
					firing[i]++
				}
			}
		}
	}

	for i, n := range firing {
		if n == 0 {
			continue
		}
		paged++
		if n > peak {
			peak, peakAt = n, w.Start.Add(time.Duration(i)*w.Step)
		}
	}
	return paged, peak, peakAt
}

// Episodes is how many separate times somebody would have been paged, summed
// over series. See the package comment for why the series episode is the unit.
func (r *Report) Episodes() int {
	var n int
	for _, s := range r.Series {
		n += s.Summary.Firing
	}
	return n
}

// Flaps is how many runs crossed the band without ever surviving For.
func (r *Report) Flaps() int {
	var n int
	for _, s := range r.Series {
		n += s.Summary.Flaps
	}
	return n
}

// Totals is every series' answers added together, so a report can say how
// much of the window carried no verdict at all. A detector with nothing firing
// and a third of its instants cold has not been shown to be quiet.
func (r *Report) Totals() Counts {
	var c Counts
	for _, s := range r.Series {
		c.Evaluated += s.Counts.Evaluated
		c.Absent += s.Counts.Absent
		c.Cold += s.Counts.Cold
		c.Undefined += s.Counts.Undefined
	}
	return c
}

// SeriesFiringTime is firing time summed over series, which counts two
// instances firing for an hour as two hours.
func (r *Report) SeriesFiringTime() time.Duration {
	var d time.Duration
	for _, s := range r.Series {
		d += s.Summary.FiringTime
	}
	return d
}

// PagedTime is the wall clock time during which at least one series was
// firing, which counts two instances firing for the same hour as one hour.
func (r *Report) PagedTime() time.Duration {
	return time.Duration(r.PagedInstants) * r.Window.Step
}

// Longest is the longest firing episode over every series, and the series it
// belonged to. The second return is false when nothing fired.
func (r *Report) Longest() (SeriesReport, episode.Episode, bool) {
	var (
		worst episode.Episode
		where SeriesReport
		found bool
	)
	for _, s := range r.Series {
		for _, e := range s.Episodes {
			if e.FiringDuration() > worst.FiringDuration() {
				worst, where, found = e, s, true
			}
		}
	}
	return where, worst, found
}

// Ratio is a duration as a fraction of the reported window, which is the
// denominator the package comment argues for. A zero-length window reports
// zero rather than dividing by it.
func (r *Report) Ratio(d time.Duration) float64 {
	window := r.Window.Reported()
	if window <= 0 {
		return 0
	}
	return float64(d) / float64(window)
}

// Thresholds are the limits a CI gate sets. A negative field is unset, since
// zero is a threshold somebody means: a detector that may never page.
type Thresholds struct {
	MaxEpisodes    int
	MaxFiringRatio float64

	// EpisodesFrom and RatioFrom are the file that set each limit, named in
	// the finding it produces. A repository with a budget per team holds a
	// detector to limits from more than one file, so "over budget" without
	// the file is a sentence a reader cannot act on.
	EpisodesFrom string
	RatioFrom    string
}

// NoThresholds is every limit unset.
var NoThresholds = Thresholds{MaxEpisodes: -1, MaxFiringRatio: -1}

// Exceeded is every threshold this report breaks, in words a CI log can print.
// Empty means the detector is within its limits, or that none were set.
func (r *Report) Exceeded(t Thresholds) []string {
	var out []string
	if t.MaxEpisodes >= 0 && r.Episodes() > t.MaxEpisodes {
		out = append(out, fmt.Sprintf("%s: %d paging episodes, limit %d%s",
			r.Detector, r.Episodes(), t.MaxEpisodes, from(t.EpisodesFrom)))
	}
	if ratio := r.Ratio(r.PagedTime()); t.MaxFiringRatio >= 0 && ratio > t.MaxFiringRatio {
		out = append(out, fmt.Sprintf("%s: paged for %s of the window, limit %s%s",
			r.Detector, percent(ratio), percent(t.MaxFiringRatio), from(t.RatioFrom)))
	}
	return out
}

// from is the file a limit came from, as a finding carries it, and nothing
// when the caller had no file to name.
func from(file string) string {
	if file == "" {
		return ""
	}
	return " (" + file + ")"
}

// Requirements is what the band needs from whatever will evaluate it, as the
// report prints them.
func (r *Report) Requirements() string {
	reqs := r.Band.Requires()
	if len(reqs) == 0 {
		return "nothing optional"
	}
	words := make([]string, 0, len(reqs))
	for _, req := range reqs {
		words = append(words, string(req))
	}
	slices.Sort(words)
	return strings.Join(words, ", ")
}
