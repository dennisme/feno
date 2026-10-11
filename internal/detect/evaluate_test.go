package detect

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/dennisme/feno/internal/episode"
	"github.com/dennisme/feno/internal/prometheus"
)

var base = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

const step = time.Minute

// at returns the timestamp of the nth evaluation on the step grid.
func at(n int) time.Time { return base.Add(time.Duration(n) * step) }

// points places one value at every step from the grid origin, which is the
// shape a range query returns when nothing is stale.
func points(vals ...float64) []prometheus.Point {
	out := make([]prometheus.Point, len(vals))
	for i, v := range vals {
		out[i] = prometheus.Point{At: at(i), Value: v}
	}
	return out
}

func values(ps []prometheus.Point) []float64 {
	out := make([]float64, len(ps))
	for i, p := range ps {
		out[i] = p.Value
	}
	return out
}

func equal(t *testing.T, got, want []float64, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

// TestWindow pins the range selector boundary directly, because inferring it
// from a band's centre would pass just as happily with the window off by one
// sample in either direction.
func TestWindow(t *testing.T) {
	ps := points(0, 1, 2, 3, 4, 5)

	tests := []struct {
		name string
		end  time.Time
		w    time.Duration
		want []float64
	}{
		{
			// Left-open: the sample exactly one window back is outside the
			// range selector, so [3m] ending at minute 3 holds minutes 1
			// through 3 and not minute 0.
			name: "a sample exactly one window back is excluded",
			end:  at(3),
			w:    3 * step,
			want: []float64{1, 2, 3},
		},
		{
			// Right-closed: the sample at the instant itself is inside.
			name: "the sample at the end is included",
			end:  at(2),
			w:    2 * step,
			want: []float64{1, 2},
		},
		{
			name: "a window reaching past the start keeps everything up to the end",
			end:  at(1),
			w:    time.Hour,
			want: []float64{0, 1},
		},
		{
			name: "a window ending before any sample is empty",
			end:  at(0).Add(-time.Second),
			w:    time.Hour,
			want: nil,
		},
		{
			name: "a window shorter than the step holds only its own end",
			end:  at(4),
			w:    step / 2,
			want: []float64{4},
		},
		{
			name: "a window ending between samples stops at the earlier one",
			end:  at(4).Add(step / 2),
			w:    2 * step,
			want: []float64{3, 4},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := values(window(ps, tc.end, tc.w))
			equal(t, got, tc.want, "window")
		})
	}
}

func TestEvaluateErrors(t *testing.T) {
	valid := Spec{Strategy: Adaptive, Sensitivity: Balanced, Window: 3 * step, MinSamples: 2}
	rng := Range{Start: at(0), End: at(3), Step: step}

	tests := []struct {
		name   string
		spec   Spec
		rng    Range
		points []prometheus.Point
		want   error
	}{
		{
			name: "an invalid spec is reported before anything is evaluated",
			spec: Spec{},
			rng:  rng,
			want: ErrStrategy,
		},
		{
			name: "a zero step cannot describe an evaluation interval",
			spec: valid,
			rng:  Range{Start: at(0), End: at(3)},
			want: ErrStep,
		},
		{
			name: "an end before the start is not a range",
			spec: valid,
			rng:  Range{Start: at(3), End: at(0), Step: step},
			want: ErrRange,
		},
		{
			name: "points out of order are a bug in whatever produced them",
			spec: valid,
			rng:  rng,
			points: []prometheus.Point{
				{At: at(2), Value: 1},
				{At: at(1), Value: 1},
			},
			want: ErrUnordered,
		},
		{
			name: "two points at the same instant are the same bug",
			spec: valid,
			rng:  rng,
			points: []prometheus.Point{
				{At: at(1), Value: 1},
				{At: at(1), Value: 2},
			},
			want: ErrUnordered,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Evaluate(tc.spec, tc.rng, tc.points)
			if !errors.Is(err, tc.want) {
				t.Errorf("Evaluate = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEvaluateStatus(t *testing.T) {
	// A flat history of 10s with one spike, a window of three steps and a
	// margin of three standard deviations. Over {10,10,10} the scale is zero,
	// so any departure at all breaches.
	spec := Spec{
		Strategy:    Adaptive,
		Sensitivity: Balanced,
		Window:      3 * step,
		MinSamples:  3,
	}

	tests := []struct {
		name      string
		spec      Spec
		points    []prometheus.Point
		rng       Range
		want      []Status
		breaching []bool
	}{
		{
			name:      "a window short of the minimum is cold, not quiet",
			spec:      spec,
			points:    points(10, 10, 10, 10),
			rng:       Range{Start: at(0), End: at(3), Step: step},
			want:      []Status{Cold, Cold, Evaluated, Evaluated},
			breaching: []bool{false, false, false, false},
		},
		{
			name:      "an instant with no point is absent",
			spec:      spec,
			points:    []prometheus.Point{{At: at(0), Value: 10}, {At: at(1), Value: 10}, {At: at(2), Value: 10}, {At: at(4), Value: 10}},
			rng:       Range{Start: at(2), End: at(4), Step: step},
			want:      []Status{Evaluated, Absent, Cold},
			breaching: []bool{false, false, false},
		},
		{
			name:      "a NaN anywhere in the window leaves the band undefined",
			spec:      spec,
			points:    points(10, math.NaN(), 10, 10, 10, 10),
			rng:       Range{Start: at(2), End: at(5), Step: step},
			want:      []Status{Undefined, Undefined, Evaluated, Evaluated},
			breaching: []bool{false, false, false, false},
		},
		{
			name:      "an infinity in the window is treated the same way",
			spec:      spec,
			points:    points(10, 10, math.Inf(1), 10, 10, 10),
			rng:       Range{Start: at(3), End: at(5), Step: step},
			want:      []Status{Undefined, Undefined, Evaluated},
			breaching: []bool{false, false, false},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Evaluate(tc.spec, tc.rng, tc.points)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d results, want %d", len(got), len(tc.want))
			}
			for i, r := range got {
				if r.Status != tc.want[i] {
					t.Errorf("result %d status = %v, want %v", i, r.Status, tc.want[i])
				}
				if r.Breaching != tc.breaching[i] {
					t.Errorf("result %d breaching = %v, want %v", i, r.Breaching, tc.breaching[i])
				}
				if want := tc.rng.Start.Add(time.Duration(i) * tc.rng.Step); !r.At.Equal(want) {
					t.Errorf("result %d at = %v, want %v", i, r.At, want)
				}
			}
		})
	}
}

// TestEvaluateCurrentValue covers the one case where the value being judged is
// not itself inside the window that produced the band: with a seasonal offset
// the band comes from earlier history, so a non-finite value at the instant
// reaches the comparison rather than poisoning the band first.
func TestEvaluateCurrentValue(t *testing.T) {
	spec := Spec{
		Strategy:    Adaptive,
		Sensitivity: Balanced,
		Window:      3 * step,
		Offsets:     []time.Duration{4 * step},
		MinSamples:  3,
	}

	// Minutes 1 through 3 hold a flat 10, which is the window for minute 7.
	history := []float64{0, 10, 10, 10, 0, 0, 0}

	tests := []struct {
		name          string
		value         float64
		wantStatus    Status
		wantBreaching bool
	}{
		{name: "inside the band", value: 10, wantStatus: Evaluated, wantBreaching: false},
		{name: "outside the band", value: 40, wantStatus: Evaluated, wantBreaching: true},
		{
			// Prometheus drops a label set whose comparison is false, and a
			// comparison against NaN is always false.
			name: "NaN never breaches", value: math.NaN(), wantStatus: Evaluated, wantBreaching: false,
		},
		{name: "positive infinity breaches", value: math.Inf(1), wantStatus: Evaluated, wantBreaching: true},
		{name: "negative infinity breaches", value: math.Inf(-1), wantStatus: Evaluated, wantBreaching: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ps := points(append(append([]float64{}, history...), tc.value)...)
			rng := Range{Start: at(7), End: at(7), Step: step}

			got, err := Evaluate(spec, rng, ps)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d results, want 1", len(got))
			}
			if got[0].Status != tc.wantStatus {
				t.Errorf("status = %v, want %v", got[0].Status, tc.wantStatus)
			}
			if got[0].Breaching != tc.wantBreaching {
				t.Errorf("breaching = %v, want %v", got[0].Breaching, tc.wantBreaching)
			}
			near(t, got[0].Band.Centre, 10, "centre")
		})
	}
}

// TestEvaluatePoolsOffsets checks that every offset contributes to one band
// rather than the last one winning.
func TestEvaluatePoolsOffsets(t *testing.T) {
	spec := Spec{
		Strategy:    Adaptive,
		Sensitivity: Balanced,
		Window:      2 * step,
		Offsets:     []time.Duration{step, 3 * step},
		MinSamples:  4,
	}

	// At minute 5 the nearer offset selects minutes 3 and 4, holding 6 and 8,
	// and the further one selects minutes 1 and 2, holding 2 and 4. The mean
	// of all four is 5, which no single one of them gives.
	ps := points(0, 2, 4, 6, 8, 100)
	rng := Range{Start: at(5), End: at(5), Step: step}

	got, err := Evaluate(spec, rng, ps)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got[0].Status != Evaluated {
		t.Fatalf("status = %v, want %v", got[0].Status, Evaluated)
	}
	near(t, got[0].Band.Centre, 5, "centre")
	// Population variance of {2,4,6,8} is (9+1+1+9)/4 = 5.
	near(t, got[0].Band.Scale, math.Sqrt(5), "scale")
}

func TestSamples(t *testing.T) {
	results := []Result{
		{Sample: episode.Sample{At: at(0)}, Status: Cold},
		{Sample: episode.Sample{At: at(1), Breaching: true}, Status: Evaluated},
		{Sample: episode.Sample{At: at(2)}, Status: Absent},
		{Sample: episode.Sample{At: at(3)}, Status: Undefined},
		{Sample: episode.Sample{At: at(4)}, Status: Evaluated},
	}

	got := Samples(results)
	want := []episode.Sample{
		{At: at(1), Breaching: true},
		{At: at(4)},
	}

	if len(got) != len(want) {
		t.Fatalf("Samples = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Samples[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestEndToEpisode is the shape the backtest has: points from the reader,
// results from here, episodes out, with no conversion at either seam.
func TestEndToEpisode(t *testing.T) {
	spec := Spec{
		Strategy:    Adaptive,
		Sensitivity: Balanced,
		Window:      3 * step,
		Offsets:     []time.Duration{4 * step},
		MinSamples:  3,
	}

	// A flat 10 through minute 6, so the band over every evaluated instant
	// has a centre of 10 and no spread at all.
	ps := points(10, 10, 10, 10, 10, 10, 10, 10, 40, 40, 10)
	rng := Range{Start: at(7), End: at(10), Step: step}

	results, err := Evaluate(spec, rng, ps)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	episodes, err := episode.Extract(Samples(results), episode.Options{Step: step})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if len(episodes) != 1 {
		t.Fatalf("got %d episodes, want 1", len(episodes))
	}
	if !episodes[0].Start.Equal(at(8)) {
		t.Errorf("start = %v, want %v", episodes[0].Start, at(8))
	}
	if !episodes[0].End.Equal(at(10)) {
		t.Errorf("end = %v, want %v", episodes[0].End, at(10))
	}
	if episodes[0].Samples != 2 {
		t.Errorf("samples = %d, want 2", episodes[0].Samples)
	}
}
