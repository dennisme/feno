package suggest

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/feno/internal/rules"
	"github.com/dennisme/feno/internal/spec"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// draftVersion stands in for the release that wrote a draft. It is fixed so
// that a golden file is not rewritten by every build.
const draftVersion = "v1.2.3"

// draftStep is the evaluation interval a drafted band is checked against. A
// minute is what a Prometheus runs by default.
const draftStep = time.Minute

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v (run with -update)", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s does not match:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// profileOf is the history every golden draft is made from: three weeks at a
// five minute step, fully covered, with a daily shape and a standard
// deviation far enough above the interquartile scale to draft the robust
// strategy. Fixed rather than read, so the golden files say what the
// decisions were and not what a server was doing.
func profileOf(query string) Profile {
	return Profile{
		Query:    query,
		From:     time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		Step:     5 * time.Minute,
		Instants: 6049,
		Series: []SeriesProfile{{
			Key:             `{service="checkout"}`,
			Samples:         6020,
			Median:          180,
			StdDev:          42,
			RobustScale:     14,
			Autocorrelation: map[time.Duration]float64{day: 0.78, week: 0.21},
		}},
	}
}

// draftOf is the whole command's pure half over one fixture directory: load,
// choose the series, decide, render.
func draftOf(t *testing.T, dir string) ([]byte, Source, Decision) {
	t.Helper()

	bundle, err := spec.Load(filepath.Join("testdata", dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := SLIs(bundle)
	if len(found) != 1 {
		t.Fatalf("%s holds %d SLIs, want 1", dir, len(found))
	}

	source, err := NewSource(found[0].SLI, found[0].File, SLOsNaming(bundle, found[0].SLI.Metadata.Name))
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	profile := profileOf(source.Query)
	decision, err := Decide(profile)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	drafted, err := Draft(source, decision, profile, draftVersion)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	return drafted, source, decision
}

func TestDraft(t *testing.T) {
	tests := []struct {
		name    string
		dir     string
		sloRef  string
		wantSay []string
	}{
		{
			name:    "a threshold SLI with one SLO over it",
			dir:     "threshold",
			sloRef:  "checkout-latency",
			wantSay: []string{"thresholdMetric", "checkout-latency"},
		},
		{
			name:    "a ratio SLI bands the total",
			dir:     "ratio",
			sloRef:  "checkout-availability",
			wantSay: []string{"ratioMetric.total"},
		},
		{
			name:   "two SLOs over one SLI leave the link to a person",
			dir:    "two-slos",
			sloRef: "",
			wantSay: []string{
				"checkout-availability-tight",
				"sloRef",
			},
		},
		{
			name:    "an SLI nobody wrote an objective over",
			dir:     "no-slo",
			sloRef:  "",
			wantSay: []string{"no SLO"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			drafted, source, _ := draftOf(t, tc.dir)
			golden(t, tc.dir+".yaml", drafted)

			if got := source.SLORef(); got != tc.sloRef {
				t.Errorf("sloRef = %q, want %q", got, tc.sloRef)
			}
			for _, want := range tc.wantSay {
				if !strings.Contains(string(drafted), want) {
					t.Errorf("the draft does not say %q:\n%s", want, drafted)
				}
			}
			if !strings.Contains(string(drafted), draftVersion) {
				t.Errorf("the draft does not name the version that wrote it:\n%s", drafted)
			}
		})
	}
}

// TestDraftLoads is the check that matters most about every golden file: the
// document this tool writes is a document this tool reads. A draft that
// needs a line edited before `feno generate` accepts it is not a draft.
func TestDraftLoads(t *testing.T) {
	for _, dir := range []string{"threshold", "ratio", "two-slos", "no-slo"} {
		t.Run(dir, func(t *testing.T) {
			drafted, source, decision := draftOf(t, dir)

			// The draft is loaded beside the SLI it came from and the
			// DataSource its metricSourceRef names, which is the tree it
			// will be committed into.
			tree := t.TempDir()
			for name, from := range map[string]string{
				"input.yaml":      filepath.Join("testdata", dir, "input.yaml"),
				"datasource.yaml": filepath.Join("testdata", "datasource.yaml"),
			} {
				data, err := os.ReadFile(from)
				if err != nil {
					t.Fatalf("reading %s: %v", from, err)
				}
				if err := os.WriteFile(filepath.Join(tree, name), data, 0o600); err != nil {
					t.Fatalf("writing %s: %v", name, err)
				}
			}
			if err := os.WriteFile(filepath.Join(tree, "detector.yaml"), drafted, 0o600); err != nil {
				t.Fatalf("writing the draft: %v", err)
			}

			bundle, err := spec.Load(tree)
			if err != nil {
				t.Fatalf("the draft does not load: %v\n%s", err, drafted)
			}
			if len(bundle.Detectors) != 1 {
				t.Fatalf("got %d detectors, want 1", len(bundle.Detectors))
			}
			detector := bundle.Detectors[0]

			// Every decision has to survive the round trip, or the draft
			// said one thing and the loader read another.
			if detector.Spec.Detection.Strategy != decision.Strategy {
				t.Errorf("strategy loaded as %q, decided %q",
					detector.Spec.Detection.Strategy, decision.Strategy)
			}
			if detector.Spec.Detection.Sensitivity != decision.Sensitivity {
				t.Errorf("sensitivity loaded as %q, decided %q",
					detector.Spec.Detection.Sensitivity, decision.Sensitivity)
			}
			if detector.Spec.Detection.Class != decision.Class {
				t.Errorf("class loaded as %q, decided %q",
					detector.Spec.Detection.Class, decision.Class)
			}
			if !slices.Equal(detector.Spec.Detection.Seasonality, decision.Seasonality) {
				t.Errorf("seasonality loaded as %v, decided %v",
					detector.Spec.Detection.Seasonality, decision.Seasonality)
			}
			if detector.Spec.SLORef != source.SLORef() {
				t.Errorf("sloRef loaded as %q, want %q", detector.Spec.SLORef, source.SLORef())
			}

			// The query has to survive it too, read back through the same
			// key the generated rule is compiled from.
			query, err := rules.Query(detector)
			if err != nil {
				t.Fatalf("reading the drafted query back: %v", err)
			}
			if query != source.Query {
				t.Errorf("query loaded as %q, want %q", query, source.Query)
			}

			// And it has to compile into a band, which is the claim that
			// the draft can be backtested without a line being edited.
			if _, err := detector.Band(draftStep); err != nil {
				t.Errorf("the drafted detector does not compile into a band: %v", err)
			}
		})
	}
}
