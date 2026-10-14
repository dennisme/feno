package episode

import (
	"errors"
	"testing"
	"time"
)

// TestOverlaps is the budget overlap, as a pure function over two timelines.
//
// The episodes come from Extract over the same grid, so every case here is
// written as two pictures of the same minutes: what the detector was doing,
// and when the error budget was burning.
func TestOverlaps(t *testing.T) {
	tests := []struct {
		name string

		// firing and burning are the two timelines, minute by minute.
		firing  string
		burning string

		// hold is the `for` the episodes are grouped at, so that a case can
		// have a flap in it.
		hold time.Duration

		// want is the coincident firing minutes of each episode, in order.
		want []int
	}{
		{
			name:    "an episode wholly inside a burn",
			firing:  "..###..",
			burning: ".#####.",
			want:    []int{3},
		},
		{
			name:    "an episode that never coincided, which is the deletion case",
			firing:  "..###..",
			burning: "......#",
			want:    []int{0},
		},
		{
			name:    "an episode overlapping one end of a burn",
			firing:  "..####.",
			burning: "...#...",
			want:    []int{1},
		},
		{
			name: "only the firing part of an episode counts",
			// Five breaching minutes with a two minute hold: the alert is
			// pending for the first two and firing for the last three, so a
			// burn across the first three coincides with one minute of
			// paging and not with three.
			firing:  ".#####.",
			burning: ".###...",
			hold:    2 * time.Minute,
			want:    []int{1},
		},
		{
			name: "a flap coincides with nothing, because nobody was woken",
			// One breaching minute against a two minute hold never fires.
			firing:  ".#.....",
			burning: ".#.....",
			hold:    2 * time.Minute,
			want:    []int{0},
		},
		{
			name:    "two episodes, one of which lined up",
			firing:  ".#..#..",
			burning: "....#..",
			want:    []int{0, 1},
		},
		{
			name:    "a burn with no firing to intersect",
			firing:  ".......",
			burning: "..###..",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			episodes, err := Extract(series(tt.firing), Options{Step: step, For: tt.hold})
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}

			overlaps, err := Overlaps(episodes, series(tt.burning), step)
			if err != nil {
				t.Fatalf("Overlaps: %v", err)
			}
			if len(overlaps) != len(tt.want) {
				t.Fatalf("got %d overlaps, want %d: %+v", len(overlaps), len(tt.want), overlaps)
			}

			for i, want := range tt.want {
				coincident := time.Duration(want) * step
				if overlaps[i].Coincident != coincident {
					t.Errorf("episode %d coincided for %s, want %s",
						i, overlaps[i].Coincident, coincident)
				}
				if overlaps[i].Touched() != (want > 0) {
					t.Errorf("episode %d touched = %v, want %v", i, overlaps[i].Touched(), want > 0)
				}
				if !overlaps[i].Episode.Start.Equal(episodes[i].Start) {
					t.Errorf("episode %d carries %s, want %s",
						i, overlaps[i].Episode.Start, episodes[i].Start)
				}
			}
		})
	}
}

// TestOverlapsRefusesAnotherGrid is the failure the whole feature turns on.
//
// An intersection of two timelines whose instants are offset from each other
// is empty however much the two conditions actually coincided. That reads as
// a detector that never lined up with an incident, which is the conclusion
// this report exists to support, so an instant that is not a whole number of
// steps from the episodes it is being intersected with is refused rather than
// silently contributing nothing.
//
// A timeline sampled at another resolution is not refusable here and is not
// checked for: its instants are still whole steps apart, so it is
// indistinguishable from one whose samples were absent in between, which a
// burn signal legitimately is. The step is one value in internal/backtest,
// passed to both reads and to both groupings, which is where that can only be
// right.
func TestOverlapsRefusesAnotherGrid(t *testing.T) {
	episodes, err := Extract(series("..###.."), Options{Step: step})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	tests := []struct {
		name     string
		timeline []Sample
		step     time.Duration
		want     error
	}{
		{
			name:     "a step of zero",
			timeline: series("..###.."),
			step:     0,
			want:     ErrStep,
		},
		{
			name: "a timeline offset from the grid by less than a step",
			timeline: []Sample{
				{At: at(2).Add(30 * time.Second), Breaching: true},
				{At: at(3).Add(30 * time.Second), Breaching: true},
			},
			step: step,
			want: ErrGrid,
		},
		{
			name: "a timeline out of order",
			timeline: []Sample{
				{At: at(3), Breaching: true},
				{At: at(2), Breaching: true},
			},
			step: step,
			want: ErrUnordered,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Overlaps(episodes, tt.timeline, tt.step)
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}
