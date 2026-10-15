package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/feno/internal/config"
)

// TestJudge is what a waiver decides, as a function of the findings, the
// waiver and an instant. The instant is a parameter so that every outcome is
// a row here rather than a clock somebody has to wait for.
func TestJudge(t *testing.T) {
	const (
		detector = "checkout-request-rate"
		file     = "teams/checkout/feno.yaml"
		finding  = "checkout-request-rate: 7 paging episodes, limit 4 (teams/checkout/feno.yaml)"
	)
	until := time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC)
	waiver := &config.Waiver{
		Reason: "black friday traffic shape",
		Until:  until,
		From:   file,
	}

	tests := []struct {
		name string
		over []string
		// now is read against the waiver's 2026-12-05.
		now          time.Time
		waiver       *config.Waiver
		failures     []string
		waivedCount  int
		inWaived     []string
		inFailures   []string
		wantExitCode int
	}{
		{
			name:         "over budget with no waiver",
			over:         []string{finding},
			now:          until.Add(-24 * time.Hour),
			failures:     []string{finding},
			wantExitCode: exitFinding,
		},
		{
			// The finding is still reported and the build does not fail,
			// which is the whole point of the mechanism.
			name:         "over budget and waived",
			over:         []string{finding},
			now:          until.Add(-24 * time.Hour),
			waiver:       waiver,
			waivedCount:  2,
			inWaived:     []string{finding, file, "2026-12-05", "black friday traffic shape"},
			wantExitCode: exitOK,
		},
		{
			name:         "over budget after the date",
			over:         []string{finding},
			now:          until,
			waiver:       waiver,
			inFailures:   []string{finding, file, "2026-12-05", "expired"},
			wantExitCode: exitFinding,
		},
		{
			// A detector somebody tuned, with the waiver still in the file.
			// The expiry is a finding of its own, so the dead line gets
			// deleted rather than sitting there until the next incident.
			name:         "within budget after the date",
			now:          until.Add(24 * time.Hour),
			waiver:       waiver,
			inFailures:   []string{detector, file, "expired"},
			wantExitCode: exitFinding,
		},
		{
			name:         "within budget and waived",
			now:          until.Add(-24 * time.Hour),
			waiver:       waiver,
			wantExitCode: exitOK,
		},
		{
			name:         "within budget with no waiver",
			now:          until.Add(-24 * time.Hour),
			wantExitCode: exitOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := judge(detector, tt.over, tt.waiver, tt.now)

			if tt.failures != nil && !equalLines(got.failures, tt.failures) {
				t.Errorf("failures = %q, want %q", got.failures, tt.failures)
			}
			if tt.inFailures != nil && len(got.failures) == 0 {
				t.Fatalf("failures = none, want a finding naming %q", tt.inFailures)
			}
			for _, want := range tt.inFailures {
				if !strings.Contains(strings.Join(got.failures, "\n"), want) {
					t.Errorf("failures = %q, want them to name %q", got.failures, want)
				}
			}
			if tt.waivedCount != len(got.waived) {
				t.Errorf("waived = %q, want %d lines", got.waived, tt.waivedCount)
			}
			for _, want := range tt.inWaived {
				if !strings.Contains(strings.Join(got.waived, "\n"), want) {
					t.Errorf("waived = %q, want them to name %q", got.waived, want)
				}
			}
			if got.code() != tt.wantExitCode {
				t.Errorf("code = %d, want %d", got.code(), tt.wantExitCode)
			}
		})
	}
}

// TestWaiverDoesNotClearTheDriftGate is the line the design draws and the
// same one clickhouse-ruler draws: `generate --check` and promtool are
// correctness, and a rule that fails either cannot do its job. A waiver is
// about a noise budget and reaches nothing else in the file.
func TestWaiverDoesNotClearTheDriftGate(t *testing.T) {
	root := t.TempDir()
	specs := filepath.Join(root, "specs")
	if err := os.MkdirAll(specs, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"checkout.yaml", "api-latency.yaml"} {
		data, err := os.ReadFile(filepath.Join("testdata", "detectors", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(specs, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A waiver covering the detector whose generated rule is about to drift,
	// with an expiry years out. Nothing it says applies to drift.
	budget := "budget:\n" +
		"  maxEpisodes: 4\n" +
		"  detectors:\n" +
		"    checkout-request-rate:\n" +
		"      waiver:\n" +
		"        reason: black friday traffic shape\n" +
		"        until: 2099-01-01\n"
	if err := os.WriteFile(filepath.Join(specs, config.Filename), []byte(budget), 0o600); err != nil {
		t.Fatal(err)
	}

	rules := filepath.Join(root, "rules")
	var stdout, stderr bytes.Buffer
	if got := run([]string{"generate", "-f", specs, "-o", rules}, &stdout, &stderr); got != exitOK {
		t.Fatalf("generate exit = %d, want %d\nstderr: %s", got, exitOK, stderr.String())
	}

	path := filepath.Join(rules, "checkout.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	edited := strings.Replace(string(data), "interval: 1m", "interval: 5m", 1)
	if edited == string(data) {
		t.Fatal("the generated file no longer contains the line this case edits")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if got := run([]string{"generate", "-f", specs, "-o", rules, "--check"}, &stdout, &stderr); got != exitFinding {
		t.Fatalf("generate --check exit = %d, want %d: a waiver cleared the drift gate\nstderr: %s",
			got, exitFinding, stderr.String())
	}
}

func equalLines(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
