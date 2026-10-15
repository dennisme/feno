package suggest

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"

	"github.com/dennisme/feno/internal/rules"
	"github.com/dennisme/feno/internal/spec"
)

// metricSource is a Prometheus metric source carrying one query, which is
// the shape every fixture in this file is built out of.
func metricSource(query string) v1.SLIMetricSource {
	return v1.SLIMetricSource{
		MetricSourceRef: "mimir-prod",
		Spec:            map[string]any{"query": query},
	}
}

func sli(name string, s v1.SLISpec) v1.SLI {
	return v1.SLI{Metadata: v1.Metadata{Name: name}, Spec: s}
}

func TestNewSourceChoosesTheSeriesToBand(t *testing.T) {
	const (
		good  = `sum(rate(http_requests_total{service="checkout",code!~"5.."}[5m]))`
		bad   = `sum(rate(http_requests_total{service="checkout",code=~"5.."}[5m]))`
		total = `sum(rate(http_requests_total{service="checkout"}[5m]))`
		level = `histogram_quantile(0.99, rate(http_request_duration_seconds_bucket[5m]))`
	)

	tests := []struct {
		name       string
		spec       v1.SLISpec
		wantQuery  string
		wantOrigin string
	}{
		{
			name:       "a threshold metric is the series",
			spec:       v1.SLISpec{ThresholdMetric: &v1.SLIMetricSpec{MetricSource: metricSource(level)}},
			wantQuery:  level,
			wantOrigin: "thresholdMetric",
		},
		{
			name: "a good over total ratio bands the total",
			spec: v1.SLISpec{RatioMetric: &v1.SLIRatioMetric{
				Good:  &v1.SLIMetricSpec{MetricSource: metricSource(good)},
				Total: &v1.SLIMetricSpec{MetricSource: metricSource(total)},
			}},
			wantQuery:  total,
			wantOrigin: "ratioMetric.total",
		},
		{
			name: "a bad over total ratio bands the total too",
			spec: v1.SLISpec{RatioMetric: &v1.SLIRatioMetric{
				Bad:   &v1.SLIMetricSpec{MetricSource: metricSource(bad)},
				Total: &v1.SLIMetricSpec{MetricSource: metricSource(total)},
			}},
			wantQuery:  total,
			wantOrigin: "ratioMetric.total",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source, err := NewSource(sli("checkout", tc.spec), "slis/checkout.yaml", nil)
			if err != nil {
				t.Fatalf("NewSource: %v", err)
			}
			if source.Query != tc.wantQuery {
				t.Errorf("query = %q, want %q", source.Query, tc.wantQuery)
			}
			if source.Origin != tc.wantOrigin {
				t.Errorf("origin = %q, want %q", source.Origin, tc.wantOrigin)
			}
			if source.Name != "checkout" || source.File != "slis/checkout.yaml" {
				t.Errorf("source = %q in %q, want checkout in slis/checkout.yaml", source.Name, source.File)
			}
		})
	}
}

func TestNewSourceRefuses(t *testing.T) {
	tests := []struct {
		name   string
		spec   v1.SLISpec
		want   error
		reason string
	}{
		{
			name:   "an SLI with neither metric",
			spec:   v1.SLISpec{},
			want:   ErrSLIShape,
			reason: "neither",
		},
		{
			name: "a raw ratio, which carries no traffic series",
			spec: v1.SLISpec{RatioMetric: &v1.SLIRatioMetric{
				Raw:     &v1.SLIMetricSpec{MetricSource: metricSource("ratio")},
				RawType: "success",
			}},
			want:   ErrSLIShape,
			reason: "raw",
		},
		{
			name: "a ratio with no total",
			spec: v1.SLISpec{RatioMetric: &v1.SLIRatioMetric{
				Good: &v1.SLIMetricSpec{MetricSource: metricSource("good")},
			}},
			want:   ErrSLIShape,
			reason: "total",
		},
		{
			name: "a metric source somewhere this target cannot read",
			spec: v1.SLISpec{ThresholdMetric: &v1.SLIMetricSpec{MetricSource: v1.SLIMetricSource{
				Type: "ClickHouse",
				Spec: map[string]any{"query": "SELECT 1"},
			}}},
			want:   rules.ErrSourceType,
			reason: "ClickHouse",
		},
		{
			name: "a metric source with no query",
			spec: v1.SLISpec{ThresholdMetric: &v1.SLIMetricSpec{MetricSource: v1.SLIMetricSource{
				MetricSourceRef: "mimir-prod",
			}}},
			want:   rules.ErrQuery,
			reason: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSource(sli("checkout", tc.spec), "slis/checkout.yaml", nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewSource error = %v, want %v", err, tc.want)
			}
			if tc.reason != "" && !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error %q does not say %q", err, tc.reason)
			}
			if !strings.Contains(err.Error(), "checkout") {
				t.Errorf("error %q does not name the SLI", err)
			}
		})
	}
}

func TestSourceSLORef(t *testing.T) {
	tests := []struct {
		name string
		slos []string
		want string
	}{
		{name: "no SLO names the SLI", slos: nil, want: ""},
		{name: "one SLO is unambiguous", slos: []string{"checkout-availability"}, want: "checkout-availability"},
		{name: "two SLOs are not", slos: []string{"checkout-availability", "checkout-tight"}, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			source, err := NewSource(
				sli("checkout", v1.SLISpec{ThresholdMetric: &v1.SLIMetricSpec{MetricSource: metricSource("up")}}),
				"slis/checkout.yaml", tc.slos)
			if err != nil {
				t.Fatalf("NewSource: %v", err)
			}
			if got := source.SLORef(); got != tc.want {
				t.Errorf("sloRef = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSLIsAndTheSLOsThatNameThem(t *testing.T) {
	bundle, err := spec.Load(filepath.Join("testdata", "two-slos"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	found := SLIs(bundle)
	if len(found) != 1 {
		t.Fatalf("got %d SLIs, want 1", len(found))
	}
	if found[0].SLI.Metadata.Name != "checkout-availability" {
		t.Errorf("SLI = %q, want checkout-availability", found[0].SLI.Metadata.Name)
	}
	if want := filepath.Join("testdata", "two-slos", "input.yaml"); found[0].File != want {
		t.Errorf("SLI file = %q, want %q", found[0].File, want)
	}

	want := []string{"checkout-availability", "checkout-availability-tight"}
	if got := SLOsNaming(bundle, "checkout-availability"); !slices.Equal(got, want) {
		t.Errorf("SLOs naming the SLI = %v, want %v", got, want)
	}
	if got := SLOsNaming(bundle, "nothing"); len(got) != 0 {
		t.Errorf("SLOs naming an unknown SLI = %v, want none", got)
	}
}
