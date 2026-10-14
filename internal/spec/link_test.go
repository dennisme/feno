package spec

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadResolvesSLORef is the link the budget overlap is computed over: a
// detector names an SLO, and what comes back carries the objective and the
// indicator whether the SLO declared one inline or named an SLI beside it.
func TestLoadResolvesSLORef(t *testing.T) {
	bundle, err := Load(filepath.Join("testdata", "linked"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	byName := map[string]Detector{}
	for _, d := range bundle.Detectors {
		byName[d.Metadata.Name] = d
	}

	tests := []struct {
		name      string
		detector  string
		slo       string
		objective string
		query     string
	}{
		{
			name:      "an SLO naming an SLI declared beside it",
			detector:  "checkout-request-rate",
			slo:       "checkout-availability",
			objective: "ratio",
			query:     `code!~"5.."`,
		},
		{
			name:      "an SLO carrying its indicator inline",
			detector:  "checkout-latency-shape",
			slo:       "checkout-latency",
			objective: "threshold",
			query:     "histogram_quantile",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detector, ok := byName[tt.detector]
			if !ok {
				t.Fatalf("detector %s not loaded, got %v", tt.detector, byName)
			}
			if detector.SLO == nil {
				t.Fatalf("%s: sloRef %q did not resolve", tt.detector, detector.Spec.SLORef)
			}
			if got := detector.SLO.Metadata.Name; got != tt.slo {
				t.Errorf("resolved SLO: got %q, want %q", got, tt.slo)
			}

			// The indicator is resolved here rather than downstream, because
			// an indicatorRef is a reference across documents and this is the
			// only place that holds every document.
			switch tt.objective {
			case "ratio":
				if detector.SLO.Indicator.RatioMetric == nil {
					t.Fatalf("%s: indicator carries no ratioMetric", tt.slo)
				}
				query, _ := detector.SLO.Indicator.RatioMetric.Good.MetricSource.Spec["query"].(string)
				if !strings.Contains(query, tt.query) {
					t.Errorf("good query %q does not contain %q", query, tt.query)
				}
			case "threshold":
				if detector.SLO.Indicator.ThresholdMetric == nil {
					t.Fatalf("%s: indicator carries no thresholdMetric", tt.slo)
				}
				query, _ := detector.SLO.Indicator.ThresholdMetric.MetricSource.Spec["query"].(string)
				if !strings.Contains(query, tt.query) {
					t.Errorf("threshold query %q does not contain %q", query, tt.query)
				}
			}
		})
	}
}

// TestLoadWithoutSLORef is the detector the whole feature exists to name: one
// nobody tied to an SLO. It loads, because a link is optional, and nothing
// downstream may mistake the absence for a resolved SLO.
func TestLoadWithoutSLORef(t *testing.T) {
	bundle, err := Load(filepath.Join("testdata", "mixed"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, d := range bundle.Detectors {
		if d.Spec.SLORef != "" {
			t.Fatalf("%s set sloRef to %q, want the fixture to set none", d.Metadata.Name, d.Spec.SLORef)
		}
		if d.SLO != nil {
			t.Errorf("%s resolved an SLO it never referenced: %v", d.Metadata.Name, d.SLO.Metadata.Name)
		}
	}
}

// TestSLORefErrors are the references that resolve to nothing. Each names the
// file and the value, because the author is looking at a directory.
func TestSLORefErrors(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		want     error
		mentions []string
	}{
		{
			name:     "sloRef names nothing",
			path:     "slo-unresolved",
			want:     ErrUnresolvedSLORef,
			mentions: []string{"slo-unresolved/detector.yaml", "checkout-request-rate", "checkout-durability", "sloRef"},
		},
		{
			name:     "sloRef names something that is not an SLO",
			path:     "slo-wrong-kind",
			want:     ErrSLORefKind,
			mentions: []string{"slo-wrong-kind/specs.yaml", "checkout-availability", "SLI", "sloRef"},
		},
		{
			name:     "the linked SLO's indicatorRef names nothing",
			path:     "slo-indicator-unresolved",
			want:     ErrUnresolvedIndicatorRef,
			mentions: []string{"checkout-availability", "checkout-success-ratio", "indicatorRef"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(filepath.Join("testdata", tt.path))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			got := filepath.ToSlash(err.Error())
			for _, want := range tt.mentions {
				if !strings.Contains(got, want) {
					t.Errorf("error %q does not mention %q", got, want)
				}
			}
		})
	}
}
