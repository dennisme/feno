package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Waiver is a detector being over its noise budget on purpose, for a stated
// reason, until a stated date.
//
// It is the third way through a pull request. The other two are tuning the
// detector, which is the outcome the gate is for, and raising the number,
// which is one reviewed line that outlives the incident that justified it.
// A waiver keeps the finding, lets the build pass, and starts failing it
// once the date passes, so the decision comes back in front of a reviewer.
//
// It is not a limit and takes no part in the merge the limits go through.
// "Stricter" does not order a waiver, so a nested file's waiver replaces the
// one above it rather than resolving against it, which is the same place
// clickhouse-ruler puts an exemption: outside the policy merge, dropping a
// finding afterwards.
//
// What is shared with that tool is the reason, the expiry, and the line that
// nothing in the file waives the offline gates. The shape is not: an
// exemption there names a check from a registry, and here there is no
// registry, only the two numbers a budget holds a detector to.
type Waiver struct {
	// Reason is why the detector is over budget. Required: a waiver nobody
	// explained cannot be reviewed when it comes up for renewal.
	Reason string `json:"reason"`

	// Until is when it stops applying. Required, because a waiver with no
	// expiry is the raised number with extra words on it.
	//
	// The instant itself is already expired, so `until: 2026-12-05` covers
	// December the fourth and not the fifth. A bare date is midnight UTC,
	// whatever the machine running the backtest is set to: a waiver that
	// expired at a different moment in CI than on a laptop would be a
	// finding that depends on where it was run. An author who needs a
	// specific local moment writes the RFC3339 form with its offset.
	Until time.Time `json:"until"`

	// From is the file it was written in, filled by resolution and empty on
	// a file as decoded. A repository with a budget per team holds a
	// detector to limits from more than one file, so "waived" without the
	// file is a sentence a reader cannot act on.
	From string `json:"-"`
}

// untilFormats are what `until` may be written as. A date is what an author
// reaches for and is read as midnight UTC; a timestamp carries its own
// offset, so a waiver can run out at a deploy rather than at midnight.
var untilFormats = []string{time.DateOnly, time.RFC3339}

// Waives reports whether this waiver still covers a detector at an instant.
// A nil waiver covers nothing, which is every detector nobody waived.
func (w *Waiver) Waives(now time.Time) bool {
	return w != nil && now.Before(w.Until)
}

// Expired reports a waiver that has run out. It is a finding of its own
// rather than silence, and a finding whether or not the detector is still
// over budget: a detector that was tuned leaves a line in the file that
// means nothing, and the build failing is what gets it deleted.
func (w *Waiver) Expired(now time.Time) bool {
	return w != nil && !now.Before(w.Until)
}

// UnmarshalJSON reads the two forms `until` may be written in, which
// time.Time does not: it takes RFC3339 alone, and a date is what an author
// writing a freeze into a file reaches for.
//
// Unknown fields are refused here too, because the decoder's own refusal
// does not reach inside a type that unmarshals itself, and a misspelled
// expiry would be a waiver that ran out the moment it was written.
func (w *Waiver) UnmarshalJSON(data []byte) error {
	var raw struct {
		Reason string `json:"reason"`
		Until  string `json:"until"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}

	w.Reason = raw.Reason
	// An empty expiry is left to validation, which can name the field in the
	// file rather than reporting it as something that would not decode.
	if raw.Until == "" {
		return nil
	}

	until, err := parseUntil(raw.Until)
	if err != nil {
		return err
	}
	w.Until = until
	return nil
}

func parseUntil(text string) (time.Time, error) {
	for _, layout := range untilFormats {
		if until, err := time.Parse(layout, text); err == nil {
			return until, nil
		}
	}
	return time.Time{}, fmt.Errorf(
		"until must be a date such as 2026-12-05 or an RFC3339 timestamp, got %q", text)
}

// validate rejects a waiver missing either of the two things a reviewer
// needs. The field path is passed in so that the message names where in the
// file it was written.
func (w *Waiver) validate(field string) error {
	if w == nil {
		return nil
	}
	if w.Reason == "" {
		return fmt.Errorf("%s.reason is required: a waiver nobody explained cannot be reviewed: %w",
			field, ErrValue)
	}
	if w.Until.IsZero() {
		return fmt.Errorf("%s.until is required: a waiver with no expiry is the raised number with extra words on it: %w",
			field, ErrValue)
	}
	return nil
}
