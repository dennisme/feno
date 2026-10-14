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
		{detector: "checkout-request-rate", episodes: intPtr(2), ratio: floatPtr(0.02)},
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
			name:    "a detector entry looser than its own file",
			dir:     "looser",
			want:    ErrValue,
			inError: []string{"budget.detectors.chatty-detector.maxEpisodes", "12", "4"},
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

// TestScope is the monorepo: a budget above the teams and a budget per team,
// resolved per detector rather than once for the run.
func TestScope(t *testing.T) {
	tree := filepath.Join("testdata", "tree")
	checkout := filepath.Join(tree, "teams", "checkout", "detectors", "checkout.yaml")
	search := filepath.Join(tree, "teams", "search", "detectors", "search.yaml")

	root := absolute(t, filepath.Join(tree, Filename))
	checkoutBudget := absolute(t, filepath.Join(tree, "teams", "checkout", Filename))
	searchBudget := absolute(t, filepath.Join(tree, "teams", "search", Filename))

	tests := []struct {
		name         string
		file         string
		detector     string
		episodes     *int
		episodesFrom string
		ratio        *float64
		ratioFrom    string
	}{
		{
			// The team's detector entry is the strictest thing on the path.
			name:         "a detector its own team named",
			file:         checkout,
			detector:     "checkout-request-rate",
			episodes:     intPtr(1),
			episodesFrom: checkoutBudget,
			ratio:        floatPtr(0.05),
			ratioFrom:    root,
		},
		{
			// Nobody named it, so it gets the team's default for one field
			// and the repository's for the other.
			name:         "a detector nobody named",
			file:         checkout,
			detector:     "other-detector",
			episodes:     intPtr(2),
			episodesFrom: checkoutBudget,
			ratio:        floatPtr(0.05),
			ratioFrom:    root,
		},
		{
			// The team asked for nine and the repository says four. Both
			// apply and the stricter binds, so the team cannot loosen what it
			// does not own. A looser entry across files is dead config rather
			// than an error, because the two files have different authors.
			name:         "a team entry looser than the repository",
			file:         search,
			detector:     "search-latency",
			episodes:     intPtr(4),
			episodesFrom: root,
			ratio:        floatPtr(0.01),
			ratioFrom:    searchBudget,
		},
	}

	scope := NewScope(tree)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := scope.Limits(tt.file, tt.detector)
			if err != nil {
				t.Fatalf("Limits: %v", err)
			}
			if !equalInt(got.MaxEpisodes, tt.episodes) {
				t.Errorf("MaxEpisodes = %s, want %s", showInt(got.MaxEpisodes), showInt(tt.episodes))
			}
			if got.EpisodesFrom != tt.episodesFrom {
				t.Errorf("EpisodesFrom = %q, want %q", got.EpisodesFrom, tt.episodesFrom)
			}
			if !equalFloat(got.MaxFiringRatio, tt.ratio) {
				t.Errorf("MaxFiringRatio = %s, want %s", showFloat(got.MaxFiringRatio), showFloat(tt.ratio))
			}
			if got.RatioFrom != tt.ratioFrom {
				t.Errorf("RatioFrom = %q, want %q", got.RatioFrom, tt.ratioFrom)
			}
		})
	}

	// Every file that bound anything is reportable, which is what lets a run
	// say what it enforced and a run with nothing say that instead.
	files := scope.Files()
	if len(files) != 3 {
		t.Errorf("Files = %v, want the three in the tree", files)
	}
}

// TestScopeStopsAtRoot is the walk's boundary. The budget above the path the
// command was pointed at is not somebody's budget by accident.
func TestScopeStopsAtRoot(t *testing.T) {
	team := filepath.Join("testdata", "tree", "teams", "search")
	scope := NewScope(team)

	got, err := scope.Limits(filepath.Join(team, "detectors", "search.yaml"), "search-latency")
	if err != nil {
		t.Fatalf("Limits: %v", err)
	}
	// The team's own entry, not the four the tree above it sets.
	if !equalInt(got.MaxEpisodes, intPtr(9)) {
		t.Errorf("MaxEpisodes = %s, want 9", showInt(got.MaxEpisodes))
	}
}

// TestScopeWithoutAnyFile is the repository that has no budget. Nothing is
// enforced and nothing is an error.
func TestScopeWithoutAnyFile(t *testing.T) {
	dir := t.TempDir()
	scope := NewScope(dir)

	got, err := scope.Limits(filepath.Join(dir, "checkout.yaml"), "checkout-request-rate")
	if err != nil {
		t.Fatalf("Limits: %v", err)
	}
	if !got.Unset() {
		t.Errorf("Limits = %+v, want nothing enforced", got)
	}
	if files := scope.Files(); len(files) != 0 {
		t.Errorf("Files = %v, want none", files)
	}
}

// TestScopeReportsMalformedFile is a file on the path that does not decode.
// The walk cannot be the place a budget is silently dropped.
func TestScopeReportsMalformedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte("budget:\n  maxEpisode: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	scope := NewScope(dir)
	if _, err := scope.Limits(filepath.Join(dir, "checkout.yaml"), "anything"); !errors.Is(err, ErrDecode) {
		t.Fatalf("Limits error = %v, want %v", err, ErrDecode)
	}
}

func absolute(t *testing.T, path string) string {
	t.Helper()
	out, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
