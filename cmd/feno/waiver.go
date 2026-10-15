package main

import (
	"fmt"
	"time"

	"github.com/dennisme/feno/internal/config"
)

// verdict is what a run does about one detector: the lines that fail the
// build, and the lines it reports without failing it.
//
// The two are separate because a waived finding and a passing detector are
// different outcomes that the exit code alone cannot tell apart. The exit
// code says whether the build stops; the lines say what a reader has to act
// on and by when.
type verdict struct {
	failures []string
	waived   []string
}

// code is the exit status a verdict asks for. A waived run exits as a
// passing one does, because the thing a waiver buys is a green build.
func (v verdict) code() int {
	if len(v.failures) > 0 {
		return exitFinding
	}
	return exitOK
}

// judge is what a waiver decides about one detector's findings at an
// instant.
//
// The instant is a parameter rather than a clock read in here, so that a
// waiver on either side of its expiry is a test rather than a wait.
func judge(detector string, over []string, waiver *config.Waiver, now time.Time) verdict {
	switch {
	case waiver.Expired(now):
		// The expiry is a finding whether or not the detector is still over
		// budget. A waiver whose date has passed on a detector somebody
		// tuned is a line that means nothing, and the build failing is what
		// gets it deleted rather than renewed out of habit.
		out := verdict{failures: []string{fmt.Sprintf("the waiver for %s in %s expired on %s: %s",
			detector, waiver.From, expiry(waiver), waiver.Reason)}}
		out.failures = append(out.failures, over...)
		return out

	case waiver.Waives(now) && len(over) > 0:
		out := verdict{}
		for _, finding := range over {
			out.waived = append(out.waived, "waived: "+finding)
		}
		out.waived = append(out.waived, fmt.Sprintf("the waiver in %s runs out on %s: %s",
			waiver.From, expiry(waiver), waiver.Reason))
		return out

	default:
		return verdict{failures: over}
	}
}

// expiry is the date a report prints, in the form that cannot be read two
// ways. A bare date in the file is midnight UTC, and printing it back as the
// bare date would hide which midnight that was.
func expiry(waiver *config.Waiver) string {
	return waiver.Until.Format(time.RFC3339)
}
