package episode

import (
	"errors"
	"testing"
	"time"
)

var base = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

const step = time.Minute

// series builds samples from a pattern, one character per evaluation at the
// step: '#' breaching, '.' present and not breaching, '_' no sample at all.
// A gap is the case the bridging rule is about, and spelling it as a hole in
// a string keeps every case in this file one line long.
func series(pattern string) []Sample {
	var out []Sample
	for i, c := range pattern {
		at := base.Add(time.Duration(i) * step)
		switch c {
		case '#':
			out = append(out, Sample{At: at, Breaching: true})
		case '.':
			out = append(out, Sample{At: at, Breaching: false})
		case '_':
		default:
			panic("unknown pattern character " + string(c))
		}
	}
	return out
}

// at returns the timestamp of the nth evaluation in a pattern.
func at(n int) time.Time { return base.Add(time.Duration(n) * step) }

func TestExtract(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		opts    Options
		want    []Episode
	}{
		{
			name:    "no samples is no episodes",
			pattern: "",
			opts:    Options{Step: step},
		},
		{
			name:    "nothing breaching is no episodes",
			pattern: "....",
			opts:    Options{Step: step},
		},
		{
			name:    "a single breaching sample with no for fires for one step",
			pattern: ".#.",
			opts:    Options{Step: step},
			want: []Episode{
				{Start: at(1), End: at(2), FiredAt: at(1), Samples: 1},
			},
		},
		{
			name:    "a run shorter than for never fires",
			pattern: ".###.",
			opts:    Options{Step: step, For: 5 * time.Minute},
			want: []Episode{
				{Start: at(1), End: at(4), Samples: 3},
			},
		},
		{
			// Prometheus fires when ts-ActiveAt >= holdDuration, so a run
			// whose span equals for reaches firing on its last sample.
			name:    "a run exactly for long fires on its last sample",
			pattern: ".####.",
			opts:    Options{Step: step, For: 3 * time.Minute},
			want: []Episode{
				{Start: at(1), End: at(5), FiredAt: at(4), Samples: 4},
			},
		},
		{
			name:    "a run longer than for fires partway through",
			pattern: "#####.",
			opts:    Options{Step: step, For: 2 * time.Minute},
			want: []Episode{
				{Start: at(0), End: at(5), FiredAt: at(2), Samples: 5},
			},
		},
		{
			name:    "one non-breaching sample separates two episodes",
			pattern: "##.##",
			opts:    Options{Step: step},
			want: []Episode{
				{Start: at(0), End: at(2), FiredAt: at(0), Samples: 2},
				{Start: at(3), End: at(5), FiredAt: at(3), Samples: 2, Open: true},
			},
		},
		{
			// The Prometheus default: an evaluation with no series is an
			// evaluation that did not match, and a pending alert that does
			// not match is deleted rather than held.
			name:    "a gap breaks a run when nothing is bridged",
			pattern: "##_##",
			opts:    Options{Step: step},
			want: []Episode{
				{Start: at(0), End: at(2), FiredAt: at(0), Samples: 2},
				{Start: at(3), End: at(5), FiredAt: at(3), Samples: 2, Open: true},
			},
		},
		{
			name:    "a gap within the tolerance bridges a run",
			pattern: "##_##",
			opts:    Options{Step: step, MaxGap: 2 * time.Minute},
			want: []Episode{
				{Start: at(0), End: at(5), FiredAt: at(0), Samples: 4, Open: true},
			},
		},
		{
			name:    "a gap wider than the tolerance still breaks a run",
			pattern: "##___##",
			opts:    Options{Step: step, MaxGap: 2 * time.Minute},
			want: []Episode{
				{Start: at(0), End: at(2), FiredAt: at(0), Samples: 2},
				{Start: at(5), End: at(7), FiredAt: at(5), Samples: 2, Open: true},
			},
		},
		{
			// Nothing in the data says what happened next, so the episode
			// ends one step past the last sample and says it is open. That
			// keeps a duration equal to samples times step either way, which
			// is what makes a firing total comparable across episodes.
			name:    "a run still breaching at the end of the range is open",
			pattern: ".###",
			opts:    Options{Step: step},
			want: []Episode{
				{Start: at(1), End: at(4), FiredAt: at(1), Samples: 3, Open: true},
			},
		},
		{
			name:    "a flap that is still open has not fired",
			pattern: ".##",
			opts:    Options{Step: step, For: 5 * time.Minute},
			want: []Episode{
				{Start: at(1), End: at(3), Samples: 2, Open: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Extract(series(tt.pattern), tt.opts)
			if err != nil {
				t.Fatalf("Extract() error = %v, want nil", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d episodes, want %d: %+v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("episode %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestExtractRejectsUnusableInput(t *testing.T) {
	tests := []struct {
		name    string
		samples []Sample
		opts    Options
		want    error
	}{
		{
			// Out of order samples mean the reader that produced them is
			// wrong. Sorting here would hide that and report a plausible
			// number from input nobody can trust.
			name: "samples out of order",
			samples: []Sample{
				{At: at(2), Breaching: true},
				{At: at(1), Breaching: true},
			},
			opts: Options{Step: step},
			want: ErrUnordered,
		},
		{
			name: "two samples at one timestamp",
			samples: []Sample{
				{At: at(1), Breaching: true},
				{At: at(1), Breaching: false},
			},
			opts: Options{Step: step},
			want: ErrUnordered,
		},
		{
			name:    "a step of zero",
			samples: []Sample{{At: at(0), Breaching: true}},
			opts:    Options{},
			want:    ErrStep,
		},
		{
			name:    "a negative step",
			samples: []Sample{{At: at(0), Breaching: true}},
			opts:    Options{Step: -time.Minute},
			want:    ErrStep,
		},
		{
			name:    "a negative for",
			samples: []Sample{{At: at(0), Breaching: true}},
			opts:    Options{Step: step, For: -time.Minute},
			want:    ErrNegative,
		},
		{
			name:    "a negative gap tolerance",
			samples: []Sample{{At: at(0), Breaching: true}},
			opts:    Options{Step: step, MaxGap: -time.Minute},
			want:    ErrNegative,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Extract(tt.samples, tt.opts)
			if !errors.Is(err, tt.want) {
				t.Errorf("Extract() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEpisodeDurations(t *testing.T) {
	e := Episode{Start: at(0), End: at(10), FiredAt: at(4), Samples: 10}

	if got, want := e.Duration(), 10*time.Minute; got != want {
		t.Errorf("Duration() = %v, want %v", got, want)
	}
	if got, want := e.FiringDuration(), 6*time.Minute; got != want {
		t.Errorf("FiringDuration() = %v, want %v", got, want)
	}
	if !e.Fired() {
		t.Error("Fired() = false, want true")
	}
	if e.Flap() {
		t.Error("Flap() = true, want false")
	}

	flap := Episode{Start: at(0), End: at(2), Samples: 2}
	if flap.FiringDuration() != 0 {
		t.Errorf("FiringDuration() = %v on an episode that never fired, want 0", flap.FiringDuration())
	}
	if !flap.Flap() {
		t.Error("Flap() = false on an episode that never fired, want true")
	}
}

func TestSummarize(t *testing.T) {
	// Three that fired, for 2, 6 and 4 minutes, and two that never did.
	episodes := []Episode{
		{Start: at(0), End: at(2), FiredAt: at(0), Samples: 2},
		{Start: at(10), End: at(16), FiredAt: at(10), Samples: 6},
		{Start: at(20), End: at(21), Samples: 1},
		{Start: at(30), End: at(34), FiredAt: at(30), Samples: 4},
		{Start: at(40), End: at(42), Samples: 2},
	}

	got := Summarize(episodes)

	if got.Matched != 5 {
		t.Errorf("Matched = %d, want 5", got.Matched)
	}
	if got.Firing != 3 {
		t.Errorf("Firing = %d, want 3", got.Firing)
	}
	if got.Flaps != 2 {
		t.Errorf("Flaps = %d, want 2", got.Flaps)
	}
	if want := 12 * time.Minute; got.FiringTime != want {
		t.Errorf("FiringTime = %v, want %v", got.FiringTime, want)
	}
	// Longest and median are over the episodes that fired, because the
	// question behind both is how long somebody was paged.
	if got.Longest.Start != at(10) {
		t.Errorf("Longest.Start = %v, want %v", got.Longest.Start, at(10))
	}
	if want := 4 * time.Minute; got.MedianFiring != want {
		t.Errorf("MedianFiring = %v, want %v", got.MedianFiring, want)
	}
}

func TestSummarizeWithNothingFiring(t *testing.T) {
	got := Summarize([]Episode{{Start: at(0), End: at(1), Samples: 1}})

	if got.Firing != 0 || got.Flaps != 1 {
		t.Errorf("Firing = %d and Flaps = %d, want 0 and 1", got.Firing, got.Flaps)
	}
	if got.FiringTime != 0 {
		t.Errorf("FiringTime = %v, want 0", got.FiringTime)
	}
	if got.Longest != (Episode{}) {
		t.Errorf("Longest = %+v, want the zero episode", got.Longest)
	}
	if got.MedianFiring != 0 {
		t.Errorf("MedianFiring = %v, want 0", got.MedianFiring)
	}
}

func TestSummarizeMedianOfTwoFiring(t *testing.T) {
	// An even count takes the mean of the middle pair, so 2 and 6 give 4.
	got := Summarize([]Episode{
		{Start: at(0), End: at(2), FiredAt: at(0), Samples: 2},
		{Start: at(10), End: at(16), FiredAt: at(10), Samples: 6},
	})

	if want := 4 * time.Minute; got.MedianFiring != want {
		t.Errorf("MedianFiring = %v, want %v", got.MedianFiring, want)
	}
}

func TestSummarizeOfNothing(t *testing.T) {
	got := Summarize(nil)

	if got != (Summary{}) {
		t.Errorf("Summarize(nil) = %+v, want the zero summary", got)
	}
}
