package episode

import (
	"errors"
	"fmt"
	"time"
)

// ErrGrid reports a timeline whose instants are not a whole number of steps
// from the episodes they are being intersected with.
//
// Two timelines read on grids offset from each other intersect in nothing,
// however much the two conditions actually coincided, and nothing is exactly
// what a detector that never lined up with an incident looks like. Returning
// a zero would make an off-by-one in a second read indistinguishable from the
// most valuable finding a backtest produces.
var ErrGrid = errors.New("timeline is not on the same grid as the episodes")

// Overlap is how much of one episode's firing time fell on a timeline that
// was true.
type Overlap struct {
	Episode    Episode
	Coincident time.Duration
}

// Touched reports an episode that coincided with the timeline at all, which
// is the count a report leads with: three of fourteen pages landed on
// something real.
func (o Overlap) Touched() bool { return o.Coincident > 0 }

// Overlaps intersects episodes with a timeline, per episode.
//
// The timeline is whatever other condition the episodes are being judged
// against, as the same [Sample] a detector's own evaluations arrive as. For a
// backtest that is the error budget burning, and an episode whose firing time
// never coincided with it is a candidate for deletion.
//
// Only the firing part of an episode is counted, from the instant it survived
// For to the one it ended at, because that is the span somebody was paged
// for. A flap contributes nothing: nobody was woken, so there was nothing for
// an incident to coincide with. Every episode still gets an entry, so the
// result is read alongside the episodes it came from.
//
// An instant with no sample is not burning. That is the same reading
// [Extract] gives an absent evaluation, and the caller is the one that knows
// how many instants carried no verdict, which is a number a report prints
// rather than a reason to refuse one here.
func Overlaps(episodes []Episode, timeline []Sample, step time.Duration) ([]Overlap, error) {
	if step <= 0 {
		return nil, fmt.Errorf("step %v: %w", step, ErrStep)
	}
	if len(episodes) == 0 {
		return nil, nil
	}

	// The grid is anchored on the first episode, because every episode came
	// out of [Extract] over one series read at one step, so they all sit on
	// the same instants.
	anchor := episodes[0].Start

	burning := make(map[int64]bool, len(timeline))
	for i, sample := range timeline {
		if i > 0 && !sample.At.After(timeline[i-1].At) {
			return nil, fmt.Errorf("sample %d at %s follows %s: %w",
				i, sample.At, timeline[i-1].At, ErrUnordered)
		}
		if sample.At.Sub(anchor)%step != 0 {
			return nil, fmt.Errorf("instant %s is not a whole %v from %s: %w",
				sample.At, step, anchor, ErrGrid)
		}
		if sample.Breaching {
			burning[sample.At.UnixNano()] = true
		}
	}

	overlaps := make([]Overlap, 0, len(episodes))
	for _, e := range episodes {
		overlap := Overlap{Episode: e}
		if e.Fired() {
			for t := e.FiredAt; t.Before(e.End); t = t.Add(step) {
				if burning[t.UnixNano()] {
					overlap.Coincident += step
				}
			}
		}
		overlaps = append(overlaps, overlap)
	}
	return overlaps, nil
}
