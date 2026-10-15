package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSuggestFlags covers every way the command refuses to run before it
// reads anything.
//
// None of these reach a server. What a profile reads from a real one is
// internal/suggest's integration test, and what the whole command does end
// to end is TestSuggestAndBacktestAgainstPrometheus in this package, both
// behind the integration tag. What is left here is the flag handling and the
// refusals, and a refusal that says the wrong thing is the difference
// between an SLI somebody fixes and one they delete.
func TestSuggestFlags(t *testing.T) {
	const (
		slis = "testdata/slis/checkout.yaml"
		url  = "http://localhost:9090"
	)

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
			inStderr: "usage: feno suggest",
		},
		{
			name:     "an SLI with no url prints usage",
			args:     []string{"--from-sli", slis},
			want:     exitUsage,
			inStderr: "usage: feno suggest",
		},
		{
			name:     "a url with no SLI prints usage",
			args:     []string{"--url", url},
			want:     exitUsage,
			inStderr: "usage: feno suggest",
		},
		{
			name:     "a window in a unit nobody has",
			args:     []string{"--from-sli", slis, "--url", url, "--window", "21f"},
			want:     exitUsage,
			inStderr: "--window",
		},
		{
			name:     "a window of nothing",
			args:     []string{"--from-sli", slis, "--url", url, "--window", "0d"},
			want:     exitUsage,
			inStderr: "--window",
		},
		{
			name:     "a step of nothing",
			args:     []string{"--from-sli", slis, "--url", url, "--step", "0s"},
			want:     exitUsage,
			inStderr: "--step",
		},
		{
			name:     "a path that holds no SLI",
			args:     []string{"--from-sli", "testdata/detectors/checkout.yaml", "--url", url},
			want:     exitUsage,
			inStderr: "no SLI to draft from",
		},
		{
			name:     "a path that is not there",
			args:     []string{"--from-sli", "testdata/nothing", "--url", url},
			want:     exitUsage,
			inStderr: "testdata/nothing",
		},
		{
			name:     "every SLI measured somewhere this target cannot read",
			args:     []string{"--from-sli", "testdata/slis/clickhouse.yaml", "--url", url},
			want:     exitFinding,
			inStderr: "ClickHouse",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := suggestCommand(t.Context(), tc.args, &stdout, &stderr); got != tc.want {
				t.Errorf("exit = %d, want %d\n%s", got, tc.want, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.inStderr) {
				t.Errorf("stderr does not say %q:\n%s", tc.inStderr, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("a refused run wrote to stdout:\n%s", stdout.String())
			}
		})
	}
}

// TestSuggestSkipsUnreachableDatasource is the degradation the backtest has,
// for the same reason: a laptop with no path to the datasource has not found
// out that nothing is worth drafting, so it says so in a code of its own
// rather than in the one that means a finding.
func TestSuggestSkipsUnreachableDatasource(t *testing.T) {
	// A server that existed and does not now, so the dial is really refused
	// rather than a fixture saying it was.
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()

	var stdout, stderr bytes.Buffer
	code := suggestCommand(t.Context(), []string{"--from-sli", "testdata/slis/checkout.yaml", "--url", srv.URL},
		&stdout, &stderr)

	if code != exitSkipped {
		t.Errorf("exit = %d, want %d\n%s", code, exitSkipped, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not reachable") {
		t.Errorf("stderr does not say the datasource was unreachable:\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("a skipped run wrote to stdout:\n%s", stdout.String())
	}
}

// TestSuggestRefusesToReplaceADraft is the check that the review a person
// did survives a second run. It also checks that the refusal happens before
// anything is read, which is why it can use a datasource that is not there.
func TestSuggestRefusesToReplaceADraft(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()

	dir := t.TempDir()
	existing := filepath.Join(dir, "checkout-availability.yaml")
	edited := []byte("# reviewed and edited by a person\n")
	if err := os.WriteFile(existing, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	args := []string{"--from-sli", "testdata/slis/checkout.yaml", "--url", srv.URL, "-o", dir}

	var stdout, stderr bytes.Buffer
	if code := suggestCommand(t.Context(), args, &stdout, &stderr); code != exitUsage {
		t.Errorf("exit = %d, want %d\n%s", code, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--force") {
		t.Errorf("stderr does not say how to replace it:\n%s", stderr.String())
	}
	after, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(edited) {
		t.Errorf("the draft already there was changed:\n%s", after)
	}

	// With --force the refusal is gone, and the run gets as far as the
	// datasource it cannot reach, which is the proof it was the file and not
	// the flags that stopped it.
	stdout.Reset()
	stderr.Reset()
	if code := suggestCommand(t.Context(), append(args, "--force"), &stdout, &stderr); code != exitSkipped {
		t.Errorf("with --force, exit = %d, want %d\n%s", code, exitSkipped, stderr.String())
	}
}

// TestSuggestDeadline is the backtest's deadline on the authoring path: a
// profile read against a server that never answers ends the run rather than
// holding a laptop open all afternoon.
func TestSuggestDeadline(t *testing.T) {
	var stdout, stderr bytes.Buffer

	got := run(t.Context(), []string{
		"suggest", "--from-sli", "testdata/slis", "--url", hung(t),
		"--request-timeout", "50ms",
	}, &stdout, &stderr)

	if got != exitSkipped {
		t.Errorf("exit code = %d, want %d\nstderr: %s", got, exitSkipped, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not reachable") {
		t.Errorf("stderr = %q, want it to say the server is not reachable", stderr.String())
	}
}
