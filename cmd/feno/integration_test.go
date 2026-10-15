//go:build integration

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisme/feno/internal/spec"
)

// TestSuggestAndBacktestAgainstPrometheus is the whole claim behind `feno
// suggest`, run end to end against a real server.
//
// The claim is that a directory holding the OpenSLO objects a team already
// keeps yields a detector that loads, carries the SLO it protects, and
// backtests without a line being edited. Every part of that is testable
// against fixtures except the part that matters: whether the document the
// command wrote is one the rest of the tool reads, after a real profile over
// real history decided what went in it. So this writes the tree a team
// would have, runs the command that drafts into it, and then runs the
// backtest over what came out, asserting only what is true whatever the
// server held.
//
// The report is checked for the SLO's name rather than for a number of
// episodes. What a budget overlap comes to depends on the history, and
// asserting a count here would be asserting something about somebody's
// monitoring. That the report names an SLO at all is the thing the draft was
// responsible for.
//
// What it needs, and skips without:
//
//	FENO_PROMETHEUS_URL     base URL of a Prometheus-compatible server
//	FENO_SLO_GOOD_QUERY     the good half of an SLO ratio over that server
//	FENO_SLO_TOTAL_QUERY    the total half, which is the series a draft bands
//
//	just backtest-history
func TestSuggestAndBacktestAgainstPrometheus(t *testing.T) {
	url := os.Getenv("FENO_PROMETHEUS_URL")
	good := os.Getenv("FENO_SLO_GOOD_QUERY")
	total := os.Getenv("FENO_SLO_TOTAL_QUERY")
	if url == "" || good == "" || total == "" {
		t.Skip("set FENO_PROMETHEUS_URL, FENO_SLO_GOOD_QUERY and FENO_SLO_TOTAL_QUERY")
	}

	// The layout spec.md Part 10 describes: a tree of specifications, and a
	// separate one for the rules feno generates.
	//
	// The generated rules sit outside the specification tree on purpose. A
	// -f path means a tree of specifications in every command here, and a
	// generated rule file is not one: pointing any of them at a directory
	// holding both refuses the whole tree as an unknown kind, which is the
	// loader doing what internal/spec says it does.
	root := t.TempDir()
	specs := filepath.Join(root, "specs")
	slis := filepath.Join(specs, "slis")
	detectors := filepath.Join(specs, "detectors")
	for _, dir := range []string{slis, detectors} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(filepath.Join(specs, "datasources.yaml"), fmt.Sprintf(`apiVersion: openslo/v1
kind: DataSource
metadata:
  name: history
spec:
  type: Prometheus
  connectionDetails:
    url: %s
`, url))

	write(filepath.Join(slis, "traffic.yaml"), fmt.Sprintf(`apiVersion: openslo/v1
kind: SLI
metadata:
  name: synthetic-availability
spec:
  ratioMetric:
    counter: true
    good:
      metricSource:
        metricSourceRef: history
        spec:
          query: %s
    total:
      metricSource:
        metricSourceRef: history
        spec:
          query: %s
---
apiVersion: openslo/v1
kind: SLO
metadata:
  name: synthetic-availability
spec:
  service: synthetic
  indicatorRef: synthetic-availability
  budgetingMethod: Occurrences
  timeWindow:
    - duration: 28d
      isRolling: true
  objectives:
    - displayName: 99.9%% of synthetic requests succeed
      target: 0.999
`, good, total))

	var stdout, stderr bytes.Buffer
	code := run([]string{"suggest",
		"--from-sli", slis,
		"-f", specs,
		"--url", url,
		"-o", detectors,
	}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("suggest exit = %d, want %d\nstdout:\n%s\nstderr:\n%s",
			code, exitOK, stdout.String(), stderr.String())
	}

	drafted := filepath.Join(detectors, "synthetic-availability.yaml")
	draft, err := os.ReadFile(drafted)
	if err != nil {
		t.Fatalf("the command reported success and wrote nothing: %v", err)
	}
	t.Logf("drafted:\n%s", draft)

	// The draft bands the ratio's total, which is the traffic the burn alert
	// cannot see fall.
	if !strings.Contains(string(draft), total) {
		t.Errorf("the draft does not band %q:\n%s", total, draft)
	}

	bundle, err := spec.Load(specs)
	if err != nil {
		t.Fatalf("the tree does not load with the draft in it: %v", err)
	}
	if len(bundle.Detectors) != 1 {
		t.Fatalf("got %d detectors, want 1", len(bundle.Detectors))
	}
	detector := bundle.Detectors[0]
	if detector.Spec.SLORef != "synthetic-availability" {
		t.Errorf("sloRef = %q, want the one SLO naming the SLI", detector.Spec.SLORef)
	}
	if detector.SLO == nil {
		t.Fatal("the drafted sloRef resolved to nothing")
	}

	// A second run refuses to throw away the draft a person would be
	// reviewing by now.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"suggest", "--from-sli", slis, "-f", specs, "--url", url, "-o", detectors},
		&stdout, &stderr); code != exitUsage {
		t.Errorf("a second suggest exit = %d, want %d\n%s", code, exitUsage, stderr.String())
	}

	// The draft also compiles, which is the other half of "without a line
	// being edited": a document the backtest reads and the emitter refuses
	// would be a draft nobody can ship. The drift gate is run straight
	// after, because a freshly generated tree that fails its own gate means
	// the two paths disagree about what the specification says.
	rules := filepath.Join(root, "rules")
	for _, args := range [][]string{
		{"generate", "-f", specs, "-o", rules},
		{"generate", "-f", specs, "-o", rules, "--check"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code != exitOK {
			t.Fatalf("%v exit = %d, want %d\nstdout:\n%s\nstderr:\n%s",
				args, code, exitOK, stdout.String(), stderr.String())
		}
	}

	// And the draft backtests as it was written, which is the claim: no line
	// edited between drafting and a report.
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"backtest", "-f", specs, "--url", url, "--from", "-2d"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("backtest exit = %d, want %d\nstdout:\n%s\nstderr:\n%s",
			code, exitOK, stdout.String(), stderr.String())
	}
	report := stdout.String()
	t.Logf("report:\n%s", report)

	if strings.Contains(report, "no linked SLO") {
		t.Errorf("the report on a drafted detector says nobody tied it to an SLO:\n%s", report)
	}
	if !strings.Contains(report, "synthetic-availability") {
		t.Errorf("the report does not name the SLO the draft linked:\n%s", report)
	}
}
