package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"

	"github.com/dennisme/feno/internal/backtest"
	"github.com/dennisme/feno/internal/config"
	"github.com/dennisme/feno/internal/spec"
)

// TestBacktestFlags covers every way the command refuses to run.
//
// None of these reach a server. What a backtest does against a real one is
// internal/backtest's integration test, behind a build tag, because this
// repository has nowhere to run a Prometheus; what is left here is the flag
// handling, and a flag parsed wrongly is a report of the wrong window.
func TestBacktestFlags(t *testing.T) {
	const detectors = "testdata/detectors/checkout.yaml"

	tests := []struct {
		name     string
		args     []string
		want     int
		inStderr string
	}{
		{
			name:     "no flags prints usage",
			args:     nil,
			want:     exitUsage,
			inStderr: "usage: feno backtest",
		},
		{
			name:     "a path with no url prints usage",
			args:     []string{"-f", detectors, "--from", "-30d"},
			want:     exitUsage,
			inStderr: "usage: feno backtest",
		},
		{
			name:     "a url with no from prints usage",
			args:     []string{"-f", detectors, "--url", "http://localhost:9090"},
			want:     exitUsage,
			inStderr: "usage: feno backtest",
		},
		{
			name:     "a from in a unit nobody has",
			args:     []string{"-f", detectors, "--url", "http://localhost:9090", "--from", "-30f"},
			want:     exitUsage,
			inStderr: "--from",
		},
		{
			name:     "a from that is neither a timestamp nor a duration",
			args:     []string{"-f", detectors, "--url", "http://localhost:9090", "--from", "last tuesday"},
			want:     exitUsage,
			inStderr: "neither an RFC 3339 timestamp nor a duration",
		},
		{
			name: "a to that is not a timestamp",
			args: []string{"-f", detectors, "--url", "http://localhost:9090",
				"--from", "-30d", "--to", "soon"},
			want:     exitUsage,
			inStderr: "--to",
		},
		{
			name:     "a url with no scheme",
			args:     []string{"-f", detectors, "--url", "localhost:9090", "--from", "-30d"},
			want:     exitUsage,
			inStderr: "scheme must be http or https",
		},
		{
			name:     "a path that is not there",
			args:     []string{"-f", "testdata/nope.yaml", "--url", "http://localhost:9090", "--from", "-30d"},
			want:     exitUsage,
			inStderr: "no such file or directory",
		},
		{
			name: "a header that is not name=value",
			args: []string{"-f", detectors, "--url", "http://localhost:9090",
				"--from", "-30d", "--header", "X-Scope-OrgID"},
			want:     exitUsage,
			inStderr: "is not name=value",
		},
		{
			// The budget is a file, and these two were how it used to be a
			// flag. Refusing them by name is the flag package's own message,
			// which names the flag that is gone.
			name: "a budget passed as a flag",
			args: []string{"-f", detectors, "--url", "http://localhost:9090",
				"--from", "-30d", "--max-episodes", "4"},
			want:     exitUsage,
			inStderr: "flag provided but not defined: -max-episodes",
		},
		{
			name: "a firing ratio passed as a flag",
			args: []string{"-f", detectors, "--url", "http://localhost:9090",
				"--from", "-30d", "--max-firing-ratio", "0.02"},
			want:     exitUsage,
			inStderr: "flag provided but not defined: -max-firing-ratio",
		},
		{
			name: "a config file that is not there",
			args: []string{"-f", detectors, "--url", "http://localhost:9090",
				"--from", "-30d", "-config", "testdata/config/nope.yaml"},
			want:     exitUsage,
			inStderr: "testdata/config/nope.yaml",
		},
		{
			name: "a config file with a misspelled field",
			args: []string{"-f", detectors, "--url", "http://localhost:9090",
				"--from", "-30d", "-config", "testdata/config/misspelled.yaml"},
			want:     exitUsage,
			inStderr: "maxEpisode",
		},
		{
			// The filter names nothing, so nothing is read. Saying so is the
			// point: a backtest that silently reported on no detectors would
			// pass a CI gate it never ran.
			name: "a detector filter that names nothing",
			args: []string{"-f", detectors, "--url", "http://localhost:9090",
				"--from", "-30d", "--detector", "nope"},
			want:     exitUsage,
			inStderr: "no detector to report on",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			got := run(append([]string{"backtest"}, tt.args...), &stdout, &stderr)
			if got != tt.want {
				t.Errorf("exit code = %d, want %d\nstderr: %s", got, tt.want, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.inStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.inStderr)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing", stdout.String())
			}
		})
	}
}

// TestTimestamp covers the two forms a window boundary is written in.
func TestTimestamp(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		in   string
		want time.Time
		err  bool
	}{
		{in: "-30d", want: now.Add(-30 * 24 * time.Hour)},
		{in: "-1w", want: now.Add(-7 * 24 * time.Hour)},
		{in: "-90m", want: now.Add(-90 * time.Minute)},
		{in: "2026-09-11T00:00:00Z", want: time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)},
		// A unit OpenSLO's shorthand does not define.
		{in: "-30f", err: true},
		// Go's own parser has no unit for a day, which is why the shorthand is
		// the one used here. Seconds are not a unit it has either.
		{in: "-30s", err: true},
		{in: "yesterday", err: true},
		{in: "", err: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := timestamp(tt.in, now)
			if tt.err {
				if err == nil {
					t.Fatalf("timestamp(%q) = %s, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("timestamp(%q): %v", tt.in, err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("timestamp(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

// TestHeaderList covers the repeated flag that makes a multi-tenant target
// reachable, including two values under one name.
func TestHeaderList(t *testing.T) {
	var headers headerList
	for _, value := range []string{"X-Scope-OrgID=team-a", "Authorization=Bearer token", "X-Scope-OrgID=team-b"} {
		if err := headers.Set(value); err != nil {
			t.Fatalf("Set(%q): %v", value, err)
		}
	}

	if got := http.Header(headers).Values("X-Scope-OrgID"); len(got) != 2 {
		t.Errorf("X-Scope-OrgID = %v, want two values", got)
	}
	if got := http.Header(headers).Get("Authorization"); got != "Bearer token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer token")
	}
	for _, bad := range []string{"", "X-Scope-OrgID", "=team-a"} {
		if err := headers.Set(bad); err == nil {
			t.Errorf("Set(%q) returned no error", bad)
		}
	}
}

// TestBacktestSkipsUnreachableDatasource is the degrade a pull request gate
// depends on: a runner with no network path to production neither passes the
// budget nor fails the build, it says the check did not run and exits in a
// code of its own.
func TestBacktestSkipsUnreachableDatasource(t *testing.T) {
	// A server that existed and does not now, so the dial is really refused
	// rather than a fixture saying it was.
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()

	root := t.TempDir()
	budget := filepath.Join(root, config.Filename)
	if err := os.WriteFile(budget, []byte("budget:\n  maxEpisodes: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	specs := filepath.Join(root, "detectors")
	if err := os.MkdirAll(specs, 0o750); err != nil {
		t.Fatal(err)
	}
	spec, err := os.ReadFile("testdata/detectors/checkout.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(specs, "checkout.yaml"), spec, 0o600); err != nil {
		t.Fatal(err)
	}

	// -f names the tree rather than the leaf, which is how a budget above
	// the specifications is asked for: the walk stops where -f points.
	var stdout, stderr bytes.Buffer
	got := run([]string{"backtest", "-f", root, "--url", srv.URL, "--from", "-30d"}, &stdout, &stderr)

	if got != exitSkipped {
		t.Errorf("exit code = %d, want %d\nstderr: %s", got, exitSkipped, stderr.String())
	}
	for _, want := range []string{"skipped", "not reachable", "offline"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
	// The budget was found by walking up from the specifications, and the
	// skip names it, because what went unchecked is the thing a reviewer has
	// to decide to accept.
	if !strings.Contains(stderr.String(), budget) {
		t.Errorf("stderr = %q, want it to name the budget at %q", stderr.String(), budget)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
}

// TestBacktestWithoutBudgetSaysSo is the repository that has no feno.yaml. The
// run is not an error, and it says on the way out that nothing was enforced,
// because a gate that quietly checks nothing is worse than no gate.
func TestBacktestWithoutBudgetSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()

	var stdout, stderr bytes.Buffer
	got := run([]string{"backtest", "-f", "testdata/detectors/checkout.yaml",
		"--url", srv.URL, "--from", "-30d"}, &stdout, &stderr)

	if got != exitSkipped {
		t.Errorf("exit code = %d, want %d\nstderr: %s", got, exitSkipped, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no "+config.Filename) {
		t.Errorf("stderr = %q, want it to say no %s was found", stderr.String(), config.Filename)
	}
}

// TestThresholds covers the two shapes a limit arrives in: a field the file
// set, including zero, and a field it left alone.
func TestThresholds(t *testing.T) {
	episodes, ratio := 0, 0.25

	tests := []struct {
		name   string
		limits config.Limits
		want   backtest.Thresholds
	}{
		{
			name: "nothing set",
			want: backtest.NoThresholds,
		},
		{
			name:   "a detector that may never page",
			limits: config.Limits{MaxEpisodes: &episodes},
			want:   backtest.Thresholds{MaxEpisodes: 0, MaxFiringRatio: -1},
		},
		{
			name:   "both limits",
			limits: config.Limits{MaxEpisodes: &episodes, MaxFiringRatio: &ratio},
			want:   backtest.Thresholds{MaxEpisodes: 0, MaxFiringRatio: 0.25},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := thresholds(tt.limits); got != tt.want {
				t.Errorf("thresholds = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestBudgetPerDetector is the monorepo in one invocation: two teams, a
// budget each, a ceiling above both, and every detector held to the strictest
// limit on its own path rather than to whichever file the run happened to
// find first.
func TestBudgetPerDetector(t *testing.T) {
	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(config.Filename, "budget:\n  maxEpisodes: 4\n  maxFiringRatio: 0.05\n")
	write(filepath.Join("teams", "checkout", config.Filename), "budget:\n  maxEpisodes: 2\n")
	write(filepath.Join("teams", "search", config.Filename), "budget:\n  maxFiringRatio: 0.01\n")

	budget, err := newBudget("", []string{root})
	if err != nil {
		t.Fatalf("newBudget: %v", err)
	}

	tests := []struct {
		name     string
		file     string
		detector string
		want     backtest.Thresholds
	}{
		{
			// The team is stricter on episodes and silent on the ratio.
			name:     "a checkout detector",
			file:     filepath.Join("teams", "checkout", "detectors", "checkout.yaml"),
			detector: "checkout-request-rate",
			want:     backtest.Thresholds{MaxEpisodes: 2, MaxFiringRatio: 0.05},
		},
		{
			name:     "a search detector",
			file:     filepath.Join("teams", "search", "detectors", "search.yaml"),
			detector: "search-latency",
			want:     backtest.Thresholds{MaxEpisodes: 4, MaxFiringRatio: 0.01},
		},
		{
			// No team file above it, so the ceiling is the whole budget.
			name:     "a detector no team owns",
			file:     filepath.Join("shared", "up.yaml"),
			detector: "target-down",
			want:     backtest.Thresholds{MaxEpisodes: 4, MaxFiringRatio: 0.05},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detector := spec.Detector{
				AnomalyDetector: spec.AnomalyDetector{
					Metadata: v1.Metadata{Name: tt.detector},
				},
				File: filepath.Join(root, tt.file),
			}

			limits, err := budget.limits(detector)
			if err != nil {
				t.Fatalf("limits: %v", err)
			}
			got := thresholds(limits)
			if got.MaxEpisodes != tt.want.MaxEpisodes || got.MaxFiringRatio != tt.want.MaxFiringRatio {
				t.Errorf("thresholds = %d episodes, %v ratio; want %d, %v",
					got.MaxEpisodes, got.MaxFiringRatio, tt.want.MaxEpisodes, tt.want.MaxFiringRatio)
			}
			// The file that set each limit is what a finding names.
			if got.EpisodesFrom == "" || got.RatioFrom == "" {
				t.Errorf("thresholds = %+v, want both limits to name a file", got)
			}
		})
	}
}
