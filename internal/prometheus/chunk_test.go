package prometheus

import (
	"errors"
	"testing"
	"time"
)

var base = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// at returns the timestamp n steps of one minute past base, which is the grid
// every case in this file is written on.
func at(n int) time.Time { return base.Add(time.Duration(n) * time.Minute) }

func TestPlanChunks(t *testing.T) {
	const step = time.Minute

	tests := []struct {
		name      string
		start     time.Time
		end       time.Time
		step      time.Duration
		maxPoints int
		want      []chunk
	}{
		{
			name:      "a range under the limit is one request",
			start:     at(0),
			end:       at(10),
			step:      step,
			maxPoints: 100,
			want:      []chunk{{start: at(0), end: at(10)}},
		},
		{
			name:      "a range exactly on the limit is one request",
			start:     at(0),
			end:       at(100),
			step:      step,
			maxPoints: 100,
			want:      []chunk{{start: at(0), end: at(100)}},
		},
		{
			// One interval past the limit needs a second request, and that
			// request holds the single point the first one could not.
			name:      "one interval over the limit splits",
			start:     at(0),
			end:       at(101),
			step:      step,
			maxPoints: 100,
			want: []chunk{
				{start: at(0), end: at(100)},
				{start: at(101), end: at(101)},
			},
		},
		{
			name:      "a remainder that does not divide evenly lands in a short last chunk",
			start:     at(0),
			end:       at(25),
			step:      step,
			maxPoints: 10,
			want: []chunk{
				{start: at(0), end: at(10)},
				{start: at(11), end: at(21)},
				{start: at(22), end: at(25)},
			},
		},
		{
			// Prometheus evaluates at start and then every step, so a step
			// wider than the range yields the one point at start and nothing
			// else. Asking for more would invent points.
			name:      "a step wider than the range is one chunk of one point",
			start:     at(0),
			end:       at(1),
			step:      time.Hour,
			maxPoints: 100,
			want:      []chunk{{start: at(0), end: at(0)}},
		},
		{
			name:      "a range not divisible by the step truncates to the grid",
			start:     at(0),
			end:       at(0).Add(2*time.Minute + 20*time.Second),
			step:      step,
			maxPoints: 100,
			want:      []chunk{{start: at(0), end: at(2)}},
		},
		{
			name:      "start equal to end is one chunk of one point",
			start:     at(0),
			end:       at(0),
			step:      step,
			maxPoints: 100,
			want:      []chunk{{start: at(0), end: at(0)}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planChunks(tc.start, tc.end, tc.step, tc.maxPoints)
			if err != nil {
				t.Fatalf("planChunks: unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d chunks %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if !got[i].start.Equal(tc.want[i].start) || !got[i].end.Equal(tc.want[i].end) {
					t.Errorf("chunk %d: got %s..%s, want %s..%s",
						i, got[i].start, got[i].end, tc.want[i].start, tc.want[i].end)
				}
			}
		})
	}
}

// The two properties stitching depends on. A boundary point served twice is a
// duplicate sample in the series, and a chunk start off the grid is a point no
// evaluation interval explains, so both are asserted over every plan rather
// than in one case.
func TestPlanChunksStitchCleanly(t *testing.T) {
	steps := []time.Duration{15 * time.Second, time.Minute, 5 * time.Minute}
	ranges := []time.Duration{time.Hour, 30 * 24 * time.Hour, 97*time.Hour + 23*time.Second}

	for _, step := range steps {
		for _, span := range ranges {
			start, end := at(0), at(0).Add(span)
			chunks, err := planChunks(start, end, step, maxPointsPerSeries)
			if err != nil {
				t.Fatalf("step %v span %v: %v", step, span, err)
			}
			for i, c := range chunks {
				if off := c.start.Sub(start) % step; off != 0 {
					t.Errorf("step %v span %v chunk %d: start %s is %v off the grid",
						step, span, i, c.start, off)
				}
				if c.end.Before(c.start) {
					t.Errorf("step %v span %v chunk %d: end %s before start %s",
						step, span, i, c.end, c.start)
				}
				if c.end.Sub(c.start)/step > time.Duration(maxPointsPerSeries) {
					t.Errorf("step %v span %v chunk %d: %s..%s exceeds the point limit",
						step, span, i, c.start, c.end)
				}
				if i == 0 {
					continue
				}
				// query_range includes its end timestamp, so the gap between
				// chunks is exactly one step: less repeats a point, more drops
				// one.
				if gap := c.start.Sub(chunks[i-1].end); gap != step {
					t.Errorf("step %v span %v chunk %d: starts %v after the previous end, want %v",
						step, span, i, gap, step)
				}
			}
		}
	}
}

func TestPlanChunksRejectsUnusableInput(t *testing.T) {
	tests := []struct {
		name  string
		start time.Time
		end   time.Time
		step  time.Duration
		want  error
	}{
		{
			name:  "end before start",
			start: at(10),
			end:   at(0),
			step:  time.Minute,
			want:  ErrRange,
		},
		{
			name:  "zero step",
			start: at(0),
			end:   at(10),
			step:  0,
			want:  ErrStep,
		},
		{
			name:  "negative step",
			start: at(0),
			end:   at(10),
			step:  -time.Minute,
			want:  ErrStep,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planChunks(tc.start, tc.end, tc.step, maxPointsPerSeries)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got error %v, want %v", err, tc.want)
			}
		})
	}
}
