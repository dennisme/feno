package detect

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/dennisme/feno/internal/episode"
	"github.com/dennisme/feno/internal/prometheus"
)

var (
	// ErrStep reports a step that cannot describe an evaluation interval.
	ErrStep = errors.New("step must be positive")

	// ErrRange reports an end before the start.
	ErrRange = errors.New("end must not be before start")

	// ErrUnordered reports points that are not strictly increasing in time.
	// Sorting them here would hide a bug in whatever produced them and
	// report a plausible band from input nobody can trust.
	ErrUnordered = errors.New("points are not in strictly increasing time order")
)

// Range is the instants to evaluate: the same start, end and step the range
// query was read at, so a result lands on every grid instant whether or not a
// sample exists there.
type Range struct {
	Start time.Time
	End   time.Time
	Step  time.Duration
}

func (r Range) validate() error {
	if r.Step <= 0 {
		return fmt.Errorf("step %v: %w", r.Step, ErrStep)
	}
	if r.End.Before(r.Start) {
		return fmt.Errorf("end %s, start %s: %w", r.End, r.Start, ErrRange)
	}
	return nil
}

// Result is one instant: the band that was in force, the value that was
// compared against it, and whether there was an answer at all.
//
// The embedded [episode.Sample] is what internal/episode reads, so a backtest
// hands results straight on through [Samples] with nothing in between.
type Result struct {
	episode.Sample

	Status Status
	Band   Band
	Value  float64
}

// Evaluate computes the band at every instant in the range and compares the
// value there against it.
//
// points are the samples the reader returned, in increasing time order,
// covering the range extended backwards by [Spec.Lookback]. Instants are
// matched to samples by exact timestamp, which is what a range query returns:
// a sample that does not sit on the grid belongs to a different query.
func Evaluate(spec Spec, r Range, points []prometheus.Point) ([]Result, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	for i := 1; i < len(points); i++ {
		if !points[i].At.After(points[i-1].At) {
			return nil, fmt.Errorf("point %d at %s follows %s: %w",
				i, points[i].At, points[i-1].At, ErrUnordered)
		}
	}

	index := make(map[int64]float64, len(points))
	for _, p := range points {
		index[p.At.UnixNano()] = p.Value
	}

	var out []Result
	for t := r.Start; !t.After(r.End); t = t.Add(r.Step) {
		value, ok := index[t.UnixNano()]
		if !ok {
			out = append(out, Result{
				Sample: episode.Sample{At: t},
				Status: Absent,
				Value:  math.NaN(),
			})
			continue
		}

		result := Result{Sample: episode.Sample{At: t}, Value: value}

		switch pool, finite := spec.pool(points, t); {
		case !finite:
			result.Status = Undefined
		case len(pool) < spec.MinSamples:
			result.Status = Cold
		default:
			result.Status = Evaluated
			result.Band = spec.band(pool)
			result.Breaching = result.Band.Breaches(value)
		}

		out = append(out, result)
	}

	return out, nil
}

// Samples reduces results to what internal/episode reads, keeping only the
// instants that carry an answer.
//
// Dropping the rest is what makes the backtest match the rule: a cold band,
// an undefined one and a missing sample all leave the alerting expression
// with no result for that series, and episode.Extract already treats a
// missing evaluation as the end of a run.
func Samples(results []Result) []episode.Sample {
	out := make([]episode.Sample, 0, len(results))
	for _, r := range results {
		if r.Status == Evaluated {
			out = append(out, r.Sample)
		}
	}
	return out
}

// pool gathers the samples behind the band at t, one window per offset.
//
// finite is false when any of them is a NaN or an infinity, which makes the
// band undefined rather than merely wide. Pooling is what makes a seasonal
// band one band: the samples from the same hour of each of the last four
// weeks describe one expected level together, and averaging four separate
// bands afterwards would not be the same number.
func (s Spec) pool(points []prometheus.Point, t time.Time) ([]float64, bool) {
	var out []float64
	for _, offset := range s.offsets() {
		for _, p := range window(points, t.Add(-offset), s.Window) {
			if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
				return nil, false
			}
			out = append(out, p.Value)
		}
	}
	return out, true
}

// window returns the points a range selector of width w ending at end would
// cover: (end-w, end], left-open and right-closed, the way Prometheus reads
// one. The sample exactly one window back is outside it.
//
// points must be sorted by time, which lets both edges be found by binary
// search. A linear scan per instant is a month of samples squared.
func window(points []prometheus.Point, end time.Time, w time.Duration) []prometheus.Point {
	lo := firstAfter(points, end.Add(-w))
	hi := firstAfter(points, end)
	return points[lo:hi]
}

// firstAfter is the index of the first point strictly after t, or len(points).
// The comparison never reports equality, so the search returns the partition
// point rather than the position of an equal element.
func firstAfter(points []prometheus.Point, t time.Time) int {
	i, _ := slices.BinarySearchFunc(points, t, func(p prometheus.Point, t time.Time) int {
		if p.At.After(t) {
			return 1
		}
		return -1
	})
	return i
}
