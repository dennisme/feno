// Package episode turns a series of band breaches into the episodes a person
// would have been paged for, and summarises them.
//
// This is the core of what a backtest reports. A band crossing on its own
// says nothing: what decides whether a detector is worth keeping is how many
// separate times it would have woken somebody, for how long, and how often it
// crossed without ever surviving `for`. Everything here is a pure function
// over samples, so the semantics below can be pinned by tests rather than
// discovered against a live database.
//
// # Matching the engine that will evaluate the rule
//
// A backtest is only useful if it agrees with the thing that will run the
// generated rule. Prometheus decides this in rules/alerting.go: an active
// alert absent from an evaluation is dropped outright while it is pending,
//
//	if a.State == StatePending || (!a.ResolvedAt.IsZero() && ts.Sub(a.ResolvedAt) > resolvedRetention) {
//		delete(r.active, fp)
//	}
//
// so the `for` timer resets on a single missed evaluation and there is no
// grace period. An alert that is present fires on
//
//	if a.State == StatePending && ts.Sub(a.ActiveAt) >= r.holdDuration {
//
// which is why a run whose span equals `for` does fire. `keep_firing_for`
// extends an alert that already reached firing and never affects a pending
// one, so it has no bearing on whether an episode counts.
//
// An evaluation where the series is missing and one where it is present but
// not breaching are indistinguishable to a rule, because either way the
// alerting expression returns nothing for that label set. So the default here
// is strict: any absence ends the run. Options.MaxGap relaxes it, because
// clickhouse-ruler evaluates `for` itself and the ClickHouse target will have
// to match whatever it does rather than what Prometheus does.
//
// # Why the intersection with another timeline is here
//
// [Overlaps] answers how much of an episode's firing time coincided with some
// other condition, which for a backtest is an SLO's error budget burning. It
// is here rather than beside the reader because it is the same kind of thing
// as everything else in this package: a pure function over samples on one
// grid, whose semantics are pinned by tests and not discovered against a
// server. What it does not do is read the second timeline or choose the
// denominator the result is reported over, both of which are the caller's.
//
// # What this package deliberately does not compute
//
// Firing time as a percentage of the window needs the range that was asked
// for, not the range the samples happen to span. A query that returned
// nothing for its first week would otherwise divide by a window that starts
// at the first sample and report a ratio that flatters the rule. The caller
// holds the requested range, so the caller computes the ratio.
//
// Distinct instances are a question about a set of series. This package reads
// one series, and the caller aggregates across them.
package episode

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

var (
	// ErrUnordered reports samples that are not strictly increasing in time.
	// Sorting them here would hide a bug in whatever produced them and report
	// a plausible number from input nobody can trust.
	ErrUnordered = errors.New("samples are not in strictly increasing time order")

	// ErrStep reports a step that cannot describe an evaluation interval.
	ErrStep = errors.New("step must be positive")

	// ErrNegative reports a negative For or MaxGap.
	ErrNegative = errors.New("durations must not be negative")
)

// Sample is one evaluation: whether the band was breached at that instant.
type Sample struct {
	At        time.Time
	Breaching bool
}

// Options are the rule's own settings, which decide how samples group into
// episodes and which of those would have paged.
type Options struct {
	// Step is the interval between evaluations. It is what tells an absent
	// sample apart from a run of consecutive ones.
	Step time.Duration

	// For is the rule's hold duration. A run has to span at least this much
	// to reach firing.
	For time.Duration

	// MaxGap is how much absence a run tolerates before it is treated as
	// ended. Zero is the Prometheus behaviour and the default.
	MaxGap time.Duration
}

// Episode is one run of consecutive breaching evaluations.
//
// End is exclusive and is the first evaluation at which the breach was over,
// which is the timestamp Prometheus would record as ResolvedAt. For a run
// still breaching when the samples ran out, End is one step past the last
// sample and Open says so. Both conventions give a duration of Samples times
// Step, which is what makes a firing total comparable across episodes.
type Episode struct {
	Start   time.Time
	End     time.Time
	FiredAt time.Time // zero if the run never survived For
	Samples int
	Open    bool
}

// Duration is how long the breach lasted.
func (e Episode) Duration() time.Duration { return e.End.Sub(e.Start) }

// Fired reports whether this run survived For and would have reached
// Alertmanager.
func (e Episode) Fired() bool { return !e.FiredAt.IsZero() }

// Flap reports a run that crossed the band without ever surviving For, so
// nobody was paged for it. A rule with many flaps and few firing episodes is
// one whose For is carrying it.
func (e Episode) Flap() bool { return !e.Fired() }

// FiringDuration is how long an alert would have been firing, which is the
// part of the episode after it survived For. It is zero for a flap.
func (e Episode) FiringDuration() time.Duration {
	if !e.Fired() {
		return 0
	}
	return e.End.Sub(e.FiredAt)
}

// Summary aggregates episodes into the numbers a report prints.
//
// Longest and MedianFiring are taken over the episodes that fired, because
// the question behind both is how long somebody was paged. A flap has no
// firing duration to rank.
type Summary struct {
	Matched      int
	Firing       int
	Flaps        int
	FiringTime   time.Duration
	Longest      Episode
	MedianFiring time.Duration
}

// Extract groups samples into episodes.
func Extract(samples []Sample, opts Options) ([]Episode, error) {
	if opts.Step <= 0 {
		return nil, fmt.Errorf("step %v: %w", opts.Step, ErrStep)
	}
	if opts.For < 0 || opts.MaxGap < 0 {
		return nil, fmt.Errorf("for %v, max gap %v: %w", opts.For, opts.MaxGap, ErrNegative)
	}
	for i := 1; i < len(samples); i++ {
		if !samples[i].At.After(samples[i-1].At) {
			return nil, fmt.Errorf("sample %d at %s follows %s: %w",
				i, samples[i].At, samples[i-1].At, ErrUnordered)
		}
	}

	var (
		out    []Episode
		cur    Episode
		inRun  bool
		lastAt time.Time
	)

	// closeRun ends the run in progress. end is exclusive, and open says whether
	// the samples simply ran out rather than the breach being seen to stop.
	closeRun := func(end time.Time, open bool) {
		cur.End = end
		cur.Open = open
		out = append(out, cur)
		cur, inRun = Episode{}, false
	}

	// startRun opens a run, and fires it immediately when For is zero, matching
	// the >= comparison Prometheus makes on the evaluation that activates it.
	startRun := func(at time.Time) {
		cur = Episode{Start: at, Samples: 1}
		if opts.For == 0 {
			cur.FiredAt = at
		}
		inRun, lastAt = true, at
	}

	for _, s := range samples {
		if !inRun {
			if s.Breaching {
				startRun(s.At)
			}
			continue
		}

		// Absence between two samples, beyond the one step that consecutive
		// evaluations are expected to be apart.
		absent := s.At.Sub(lastAt) - opts.Step

		if absent > opts.MaxGap {
			// Nothing was observed for longer than the run tolerates, so it
			// ended at the evaluation after its last sample. What happened
			// during the absence is unknown, which is why the new run below
			// starts at this sample rather than reaching back into the gap.
			closeRun(lastAt.Add(opts.Step), false)
			if s.Breaching {
				startRun(s.At)
			}
			continue
		}

		if !s.Breaching {
			closeRun(s.At, false)
			continue
		}

		cur.Samples++
		lastAt = s.At
		if cur.FiredAt.IsZero() && s.At.Sub(cur.Start) >= opts.For {
			cur.FiredAt = s.At
		}
	}

	if inRun {
		closeRun(lastAt.Add(opts.Step), true)
	}
	return out, nil
}

// Summarize aggregates a set of episodes from one series.
func Summarize(episodes []Episode) Summary {
	s := Summary{Matched: len(episodes)}

	firing := make([]time.Duration, 0, len(episodes))
	for _, e := range episodes {
		if e.Flap() {
			s.Flaps++
			continue
		}
		s.Firing++
		d := e.FiringDuration()
		s.FiringTime += d
		firing = append(firing, d)
		if d > s.Longest.FiringDuration() {
			s.Longest = e
		}
	}

	s.MedianFiring = median(firing)
	return s
}

// median of an even count is the mean of the middle pair, so a report does
// not have to explain why it picked one of two equally central episodes.
func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := slices.Clone(ds)
	slices.Sort(sorted)

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
