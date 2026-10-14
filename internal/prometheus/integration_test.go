//go:build integration

package prometheus

import (
	"context"
	"os"
	"strconv"
	"testing"
)

// TestProbeAgainstPrometheus is the only test in this package that talks to a
// server.
//
// It exists because no fixture can prove what the probe depends on. The probe
// decides a capability by reading a parse error and finding the function named
// in it, and a fixture carrying that sentence only repeats whatever was
// written into it. The sentence belongs to a release of somebody else's
// parser, and it has already been wrong once: Prometheus answers a function it
// has behind the feature flag with "is not enabled", not with the "unknown
// function with name" that a parser lacking the function entirely answers.
//
// Three things are under test, and the first two are the same assertion run
// against two servers:
//
//   - a server started with the feature flag answers the probe, so the
//     capability is reported present
//   - a server started without it rejects the probe in a way this package
//     recognises, so the capability is reported absent rather than returned as
//     a failure
//   - the rejection recognised is the specific one. Any other rejection is a
//     failure from Probe, so a parser that rephrases its error turns this test
//     red instead of quietly changing the rules feno writes.
//
// What it needs, and skips without:
//
//	FENO_PROMETHEUS_URL                   base URL of a Prometheus-compatible
//	                                      server, such as http://localhost:9090
//	FENO_EXPECT_EXPERIMENTAL_FUNCTIONS    whether that server was started with
//	                                      --enable-feature=promql-experimental-functions,
//	                                      as true or false
//
//	go test -tags integration ./internal/prometheus/
//
// `just smoketest` runs it against both halves of smoketest/compose.yaml,
// which is where the two servers come from.
func TestProbeAgainstPrometheus(t *testing.T) {
	url := os.Getenv("FENO_PROMETHEUS_URL")
	if url == "" {
		t.Skip("set FENO_PROMETHEUS_URL to the base URL of a Prometheus-compatible server")
	}
	// The expectation is required rather than defaulted. A probe that is only
	// asserted not to fail would pass against a server answering either way,
	// which is the whole question.
	expected := os.Getenv("FENO_EXPECT_EXPERIMENTAL_FUNCTIONS")
	if expected == "" {
		t.Skip("set FENO_EXPECT_EXPERIMENTAL_FUNCTIONS to whether that server has the flag")
	}
	want, err := strconv.ParseBool(expected)
	if err != nil {
		t.Fatalf("FENO_EXPECT_EXPERIMENTAL_FUNCTIONS=%q: %v", expected, err)
	}

	client, err := New(url)
	if err != nil {
		t.Fatalf("New(%q): %v", url, err)
	}

	capabilities, err := client.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if capabilities.ExperimentalFunctions != want {
		t.Errorf("ExperimentalFunctions = %t, want %t: %s was started %s the feature flag",
			capabilities.ExperimentalFunctions, want, url, with(want))
	}
}

// with names which server this was, so a failure says which of the two is
// configured wrongly rather than only which answer came back.
func with(flag bool) string {
	if flag {
		return "with"
	}
	return "without"
}
