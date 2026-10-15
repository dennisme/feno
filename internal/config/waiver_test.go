package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadWaiver is both forms of an expiry: the date an author reaches for,
// read as midnight UTC, and the timestamp that carries its own offset.
func TestLoadWaiver(t *testing.T) {
	path := filepath.Join("testdata", "waiver", Filename)
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		detector string
		reason   string
		until    time.Time
	}{
		{
			detector: "checkout-request-rate",
			reason:   "black friday traffic shape, retuning after the freeze",
			until:    time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC),
		},
		{
			detector: "search-latency",
			reason:   "the index rebuild pages until it finishes",
			until:    time.Date(2026, 11, 2, 18, 0, 0, 0, time.FixedZone("", 60*60)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.detector, func(t *testing.T) {
			limits := file.LimitsFor(tt.detector)
			if limits.Waiver == nil {
				t.Fatalf("LimitsFor(%q) carries no waiver", tt.detector)
			}
			if got := limits.Waiver.Reason; got != tt.reason {
				t.Errorf("Reason = %q, want %q", got, tt.reason)
			}
			if got := limits.Waiver.Until; !got.Equal(tt.until) {
				t.Errorf("Until = %s, want %s", got.Format(time.RFC3339), tt.until.Format(time.RFC3339))
			}
			// The finding names the file the waiver was written in, which in
			// a repository with a budget per team is often not the one the
			// author has open.
			if got := limits.Waiver.From; got != path {
				t.Errorf("From = %q, want %q", got, path)
			}
		})
	}

	// A detector nobody waived gets the file's limits and no waiver.
	if limits := file.LimitsFor("unnamed-detector"); limits.Waiver != nil {
		t.Errorf("LimitsFor(unnamed-detector) carries %+v, want no waiver", limits.Waiver)
	}
}

// TestLoadWaiverInvalid is every waiver that cannot mean what it says. Each
// is refused at load rather than resolved into something weaker, because a
// waiver nobody can review is the raised number with extra words on it.
func TestLoadWaiverInvalid(t *testing.T) {
	tests := []struct {
		name    string
		dir     string
		want    error
		inError []string
	}{
		{
			name:    "a waiver nobody explained",
			dir:     "no-reason",
			want:    ErrValue,
			inError: []string{"budget.detectors.checkout-request-rate.waiver.reason"},
		},
		{
			name:    "a waiver with no expiry",
			dir:     "no-until",
			want:    ErrValue,
			inError: []string{"budget.detectors.checkout-request-rate.waiver.until"},
		},
		{
			name:    "an expiry that is not a date",
			dir:     "bad-until",
			want:    ErrDecode,
			inError: []string{"next friday"},
		},
		{
			// A waiver over a file's defaults waives every detector the file
			// governs, including the ones nobody looked at.
			name:    "a waiver on the whole file",
			dir:     "budget-waiver",
			want:    ErrValue,
			inError: []string{"budget.waiver", "detector"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(filepath.Join("testdata", tt.dir, Filename))
			if !errors.Is(err, tt.want) {
				t.Fatalf("Load error = %v, want %v", err, tt.want)
			}
			for _, want := range tt.inError {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestWaiverDecision is the decision the gate turns on, as a function of a
// waiver and an instant. The instant is a parameter so that "expired" is a
// table rather than a clock.
func TestWaiverDecision(t *testing.T) {
	until := time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC)
	waiver := &Waiver{Reason: "black friday traffic shape", Until: until}

	tests := []struct {
		name    string
		waiver  *Waiver
		now     time.Time
		waives  bool
		expired bool
	}{
		{
			name:   "no waiver",
			waiver: nil,
			now:    until.Add(-time.Hour),
		},
		{
			name:   "the day before",
			waiver: waiver,
			now:    until.Add(-24 * time.Hour),
			waives: true,
		},
		{
			// The instant itself is expired, so `until: 2026-12-05` covers
			// December the fourth and not the fifth.
			name:    "the instant itself",
			waiver:  waiver,
			now:     until,
			expired: true,
		},
		{
			name:    "the day after",
			waiver:  waiver,
			now:     until.Add(24 * time.Hour),
			expired: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.waiver.Waives(tt.now); got != tt.waives {
				t.Errorf("Waives(%s) = %t, want %t", tt.now.Format(time.RFC3339), got, tt.waives)
			}
			if got := tt.waiver.Expired(tt.now); got != tt.expired {
				t.Errorf("Expired(%s) = %t, want %t", tt.now.Format(time.RFC3339), got, tt.expired)
			}
		})
	}
}

// TestScopeWaiverNearestWins is a waiver sitting outside the merge the limits
// go through. The team's waiver replaces the repository's rather than being
// tightened against it, and the limits on the same detector still resolve to
// the stricter of the two files.
func TestScopeWaiverNearestWins(t *testing.T) {
	tree := filepath.Join("testdata", "waiver-tree")
	checkout := filepath.Join(tree, "teams", "checkout", "detectors", "checkout.yaml")

	root := absolute(t, filepath.Join(tree, Filename))
	team := absolute(t, filepath.Join(tree, "teams", "checkout", Filename))

	tests := []struct {
		name     string
		detector string
		reason   string
		until    time.Time
		from     string
		episodes *int
	}{
		{
			name:     "a detector both files waived",
			detector: "checkout-request-rate",
			reason:   "checkout's own black friday shape",
			until:    time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC),
			from:     team,
			episodes: intPtr(2),
		},
		{
			name:     "a detector only the repository waived",
			detector: "other-detector",
			reason:   "the platform team waived one nobody below it names",
			until:    time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			from:     root,
			episodes: intPtr(2),
		},
	}

	scope := NewScope(tree)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits, err := scope.Limits(checkout, tt.detector)
			if err != nil {
				t.Fatalf("Limits: %v", err)
			}
			if limits.Waiver == nil {
				t.Fatalf("Limits(%q) carries no waiver", tt.detector)
			}
			if got := limits.Waiver.Reason; got != tt.reason {
				t.Errorf("Reason = %q, want %q", got, tt.reason)
			}
			if got := limits.Waiver.Until; !got.Equal(tt.until) {
				t.Errorf("Until = %s, want %s", got.Format(time.RFC3339), tt.until.Format(time.RFC3339))
			}
			if got := limits.Waiver.From; got != tt.from {
				t.Errorf("From = %q, want %q", got, tt.from)
			}
			if !equalInt(limits.MaxEpisodes, tt.episodes) {
				t.Errorf("MaxEpisodes = %s, want %s", showInt(limits.MaxEpisodes), showInt(tt.episodes))
			}
		})
	}
}
