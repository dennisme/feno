package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFull(t *testing.T) {
	file, err := Load(filepath.Join("testdata", "full", Filename))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := len(file.Targets); got != 1 {
		t.Fatalf("got %d targets, want 1", got)
	}
	if got := file.Targets[0].Engine; got != "prometheus" {
		t.Errorf("target engine = %q, want %q", got, "prometheus")
	}
	if got := file.Targets[0].Output; got != "rules/anomaly/" {
		t.Errorf("target output = %q, want %q", got, "rules/anomaly/")
	}

	target, ok := file.Target("prometheus")
	if !ok {
		t.Fatalf("Target(prometheus) not found in %v", file.Targets)
	}
	if got := target.Output; got != "rules/anomaly/" {
		t.Errorf("Target(prometheus).Output = %q, want %q", got, "rules/anomaly/")
	}
	if _, ok := file.Target("clickhouse"); ok {
		t.Error("Target(clickhouse) found a target the file does not declare")
	}
}

// TestBudgetFor covers the merge the budget shape is for: a repository
// default, a detector that overrides one field of it, and a detector the
// file does not name.
func TestBudgetFor(t *testing.T) {
	file, err := Load(filepath.Join("testdata", "full", Filename))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		detector string
		episodes *int
		ratio    *float64
	}{
		{detector: "unnamed-detector", episodes: intPtr(4), ratio: floatPtr(0.02)},
		// One field named, so the other stays the repository default.
		{detector: "checkout-request-rate", episodes: intPtr(12), ratio: floatPtr(0.02)},
		// Zero is a limit somebody means: a detector that may never page.
		{detector: "quiet-detector", episodes: intPtr(0), ratio: floatPtr(0)},
	}

	for _, tt := range tests {
		t.Run(tt.detector, func(t *testing.T) {
			got := file.Budget.For(tt.detector)
			if !equalInt(got.MaxEpisodes, tt.episodes) {
				t.Errorf("MaxEpisodes = %s, want %s", showInt(got.MaxEpisodes), showInt(tt.episodes))
			}
			if !equalFloat(got.MaxFiringRatio, tt.ratio) {
				t.Errorf("MaxFiringRatio = %s, want %s", showFloat(got.MaxFiringRatio), showFloat(tt.ratio))
			}
			if got.Unset() {
				t.Error("Unset reported no limits, want limits")
			}
		})
	}
}

// TestBudgetUnset is the file that asked for no budget. Every detector comes
// back with nothing to enforce, which is what keeps a repository with no
// budget from failing a backtest.
func TestBudgetUnset(t *testing.T) {
	file, err := Load(filepath.Join("testdata", "empty", Filename))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	limits := file.Budget.For("checkout-request-rate")
	if !limits.Unset() {
		t.Errorf("For() = %+v, want no limits", limits)
	}
}

// TestLoadErrors covers the malformed file. Every message names the file and
// the field, because the author fixes a file by opening it at a line.
func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		dir     string
		want    error
		inError []string
	}{
		{
			name:    "a misspelled field",
			dir:     "misspelled",
			want:    ErrDecode,
			inError: []string{"maxEpisode", filepath.Join("testdata", "misspelled", Filename)},
		},
		{
			name:    "a ratio outside zero to one",
			dir:     "bad-ratio",
			want:    ErrValue,
			inError: []string{"budget.maxFiringRatio", "1.5"},
		},
		{
			name:    "a negative episode count",
			dir:     "negative",
			want:    ErrValue,
			inError: []string{"budget.maxEpisodes", "-1"},
		},
		{
			name:    "a target with no output directory",
			dir:     "no-output",
			want:    ErrValue,
			inError: []string{"targets[0].output"},
		},
		{
			name:    "two targets for one engine",
			dir:     "duplicate-engine",
			want:    ErrDuplicateEngine,
			inError: []string{"targets[1].engine", "prometheus"},
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

// TestLoadMissingFile is the file somebody asked for by name and that is not
// there. A missing file is an error here; whether a missing file matters at
// all is [Find]'s question, not this one's.
func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join("testdata", "nope", Filename)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load of a file that is not there returned no error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name %q", err, path)
	}
}

// TestFind walks up from a specification directory, which is what makes the
// budget hard to drop: the file is found from wherever the command was
// invoked under the repository.
func TestFind(t *testing.T) {
	root := t.TempDir()
	specs := filepath.Join(root, "alerting", "detectors")
	if err := os.MkdirAll(specs, 0o750); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "alerting", Filename)
	if err := os.WriteFile(want, []byte("budget:\n  maxEpisodes: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok := Find(specs)
	if !ok {
		t.Fatalf("Find(%s) found nothing", specs)
	}
	if got != want {
		t.Errorf("Find = %q, want %q", got, want)
	}

	// The nearest file wins, so a repository may hold one budget per
	// specification tree.
	nearer := filepath.Join(specs, Filename)
	if err := os.WriteFile(nearer, []byte("budget:\n  maxEpisodes: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := Find(specs); got != nearer {
		t.Errorf("Find = %q, want the nearer %q", got, nearer)
	}
}

// TestFindNothing is a tree with no file in it. The walk stops at the root of
// the filesystem rather than reporting the error of having reached it.
func TestFindNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "detectors")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if got, ok := Find(dir); ok {
		t.Errorf("Find = %q, want nothing", got)
	}
}

// TestFindFromFile is the common invocation: -f names a file rather than a
// directory, and the walk starts beside it.
func TestFindFromFile(t *testing.T) {
	root := t.TempDir()
	spec := filepath.Join(root, "checkout.yaml")
	if err := os.WriteFile(spec, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, Filename)
	if err := os.WriteFile(want, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok := Find(spec)
	if !ok {
		t.Fatalf("Find(%s) found nothing", spec)
	}
	if got != want {
		t.Errorf("Find = %q, want %q", got, want)
	}
}

func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }

func equalInt(got, want *int) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

func equalFloat(got, want *float64) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

func showInt(v *int) string {
	if v == nil {
		return "unset"
	}
	return fmt.Sprint(*v)
}

func showFloat(v *float64) string {
	if v == nil {
		return "unset"
	}
	return fmt.Sprint(*v)
}
