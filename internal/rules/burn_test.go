package rules

import (
	"errors"
	"math"
	"strings"
	"testing"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"

	"github.com/dennisme/feno/internal/spec"
)

// detector is the shape every case here shares: one Prometheus detector,
// optionally linked to an SLO. The band is irrelevant to a burn signal, so it
// is left at the simplest thing that validates.
func detector(slo *spec.SLO) spec.Detector {
	return spec.Detector{
		File: "detectors/checkout.yaml",
		SLO:  slo,
		AnomalyDetector: spec.AnomalyDetector{
			APIVersion: spec.APIVersion,
			Kind:       spec.Kind,
			Metadata:   v1.Metadata{Name: "checkout-request-rate"},
			Spec: spec.DetectorSpec{
				Indicator: v1.SLOIndicatorInline{
					Metadata: v1.Metadata{Name: "checkout-requests"},
					Spec: v1.SLISpec{ThresholdMetric: &v1.SLIMetricSpec{
						MetricSource: v1.SLIMetricSource{
							Type: SourceType,
							Spec: map[string]any{queryKey: "sum(rate(http_requests_total[5m]))"},
						},
					}},
				},
				Detection: spec.Detection{Strategy: "adaptive", Sensitivity: "balanced", Class: spec.ClassRequests},
			},
		},
	}
}

// metricSource is one metric source carrying a query, as an SLI holds it.
func metricSource(query string) v1.SLIMetricSource {
	return v1.SLIMetricSource{Type: SourceType, Spec: map[string]any{queryKey: query}}
}

// linked is an SLO with one objective over the given indicator.
func linked(name string, indicator v1.SLISpec, objective v1.SLOObjective) *spec.SLO {
	return &spec.SLO{
		SLO: v1.SLO{
			Metadata: v1.Metadata{Name: name},
			Spec: v1.SLOSpec{
				Service:         "checkout",
				BudgetingMethod: v1.SLOBudgetingMethodOccurrences,
				Objectives:      []v1.SLOObjective{objective},
			},
		},
		Indicator: indicator,
	}
}

func ratio(good, bad, total string) v1.SLISpec {
	metric := &v1.SLIRatioMetric{Counter: true}
	if good != "" {
		metric.Good = &v1.SLIMetricSpec{MetricSource: metricSource(good)}
	}
	if bad != "" {
		metric.Bad = &v1.SLIMetricSpec{MetricSource: metricSource(bad)}
	}
	if total != "" {
		metric.Total = &v1.SLIMetricSpec{MetricSource: metricSource(total)}
	}
	return v1.SLISpec{RatioMetric: metric}
}

func threshold(query string) v1.SLISpec {
	return v1.SLISpec{ThresholdMetric: &v1.SLIMetricSpec{MetricSource: metricSource(query)}}
}

func floatOf(f float64) *float64 { return &f }

func ref(name string) *string { return &name }

// TestBurnQuery is the second query a backtest reads: the burn signal for the
// SLO a detector links to, as PromQL plus the comparison that says the budget
// was burning.
//
// The comparison is a value here rather than a `> bool 1` inside the query on
// purpose. A query that returned 0 and 1 would report a divide by zero as not
// burning, because NaN compares false, and a burn signal that is quietly
// false where the SLI is undefined is the one failure mode that would make
// every overlap number flatter the detector.
func TestBurnQuery(t *testing.T) {
	tests := []struct {
		name      string
		slo       *spec.SLO
		query     string
		operator  v1.Operator
		threshold float64
		objective string

		// comparison is the whole test as a report prints it.
		comparison string
	}{
		{
			name: "a good over total ratio",
			slo: linked("checkout-availability",
				ratio("sum(rate(good[5m]))", "", "sum(rate(total[5m]))"),
				v1.SLOObjective{Target: floatOf(0.999)}),
			// The error ratio over the allowance the objective leaves, which
			// is the burn rate: above one the budget is being spent faster
			// than the objective permits.
			query:      "(1 - ((sum(rate(good[5m]))) / (sum(rate(total[5m]))))) / (1 - 0.999)",
			operator:   v1.OperatorGT,
			threshold:  1,
			objective:  "99.9%",
			comparison: "> 1",
		},
		{
			name: "a bad over total ratio",
			slo: linked("checkout-availability",
				ratio("", "sum(rate(bad[5m]))", "sum(rate(total[5m]))"),
				v1.SLOObjective{Target: floatOf(0.99)}),
			query:      "((sum(rate(bad[5m]))) / (sum(rate(total[5m])))) / (1 - 0.99)",
			operator:   v1.OperatorGT,
			threshold:  1,
			objective:  "99%",
			comparison: "> 1",
		},
		{
			name: "a target written as a percentage",
			slo: linked("checkout-availability",
				ratio("good", "", "total"),
				v1.SLOObjective{TargetPercent: floatOf(99.9)}),
			query:     "(1 - ((good) / (total))) / (1 - 0.999)",
			operator:  v1.OperatorGT,
			threshold: 1,
			objective: "99.9%",
		},
		{
			name: "a threshold objective is burning when it is violated",
			slo: linked("checkout-latency",
				threshold("histogram_quantile(0.99, rate(b[5m]))"),
				v1.SLOObjective{Operator: v1.OperatorLT, Value: floatOf(0.5), Target: floatOf(0.99)}),
			// The SLI is good below 0.5, so the budget burns at or above it.
			// The operator is the objective's, negated.
			query:      "histogram_quantile(0.99, rate(b[5m]))",
			operator:   v1.OperatorGTE,
			threshold:  0.5,
			objective:  "lt 0.5",
			comparison: ">= 0.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			burn, err := BurnQuery(detector(tt.slo))
			if err != nil {
				t.Fatalf("BurnQuery: %v", err)
			}
			if burn.Query != tt.query {
				t.Errorf("query:\n got %s\nwant %s", burn.Query, tt.query)
			}
			if burn.Operator != tt.operator {
				t.Errorf("operator: got %q, want %q", burn.Operator, tt.operator)
			}
			if burn.Threshold != tt.threshold {
				t.Errorf("threshold: got %v, want %v", burn.Threshold, tt.threshold)
			}
			if burn.SLO != tt.slo.Metadata.Name {
				t.Errorf("SLO: got %q, want %q", burn.SLO, tt.slo.Metadata.Name)
			}
			if burn.Objective != tt.objective {
				t.Errorf("objective: got %q, want %q", burn.Objective, tt.objective)
			}
			if tt.comparison != "" && burn.Comparison() != tt.comparison {
				t.Errorf("comparison: got %q, want %q", burn.Comparison(), tt.comparison)
			}
		})
	}
}

// TestBurningAt is the comparison, applied. A value the server could not
// compute is not a quiet budget.
func TestBurningAt(t *testing.T) {
	burn := Burn{Operator: v1.OperatorGT, Threshold: 1}

	tests := []struct {
		name    string
		value   float64
		burning bool
		known   bool
	}{
		{name: "above the threshold", value: 1.4, burning: true, known: true},
		{name: "at the threshold", value: 1, burning: false, known: true},
		{name: "below the threshold", value: 0.2, burning: false, known: true},
		{name: "a divide by zero", value: math.NaN(), burning: false, known: false},
		{name: "an infinity", value: math.Inf(1), burning: false, known: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			burning, known := burn.At(tt.value)
			if burning != tt.burning || known != tt.known {
				t.Errorf("At(%v) = (%v, %v), want (%v, %v)",
					tt.value, burning, known, tt.burning, tt.known)
			}
		})
	}
}

// TestBurnQueryRefusals are the SLOs a burn signal cannot be compiled from.
// Each is refused rather than approximated, because a plausible wrong burn
// signal makes every overlap number a lie.
func TestBurnQueryRefusals(t *testing.T) {
	tests := []struct {
		name string
		slo  *spec.SLO
		want error
	}{
		{
			name: "a detector linked to no SLO",
			slo:  nil,
			want: ErrUnlinked,
		},
		{
			name: "two objectives, so two budgets",
			slo: func() *spec.SLO {
				slo := linked("checkout-availability",
					ratio("good", "", "total"), v1.SLOObjective{Target: floatOf(0.99)})
				slo.Spec.Objectives = append(slo.Spec.Objectives, v1.SLOObjective{Target: floatOf(0.999)})
				return slo
			}(),
			want: ErrObjectives,
		},
		{
			name: "a composite objective, whose budget is somebody else's sum",
			slo: linked("checkout-availability", ratio("good", "", "total"),
				v1.SLOObjective{Target: floatOf(0.99), IndicatorRef: ref("checkout-availability")}),
			want: ErrComposite,
		},
		{
			name: "a ratio with no total to divide by",
			slo: linked("checkout-availability", ratio("good", "", ""),
				v1.SLOObjective{Target: floatOf(0.99)}),
			want: ErrRatio,
		},
		{
			name: "a ratio carrying both good and bad",
			slo: linked("checkout-availability", ratio("good", "bad", "total"),
				v1.SLOObjective{Target: floatOf(0.99)}),
			want: ErrRatio,
		},
		{
			name: "a ratio target of one, which leaves no budget to burn",
			slo: linked("checkout-availability", ratio("good", "", "total"),
				v1.SLOObjective{Target: floatOf(1)}),
			want: ErrTarget,
		},
		{
			name: "a ratio with no target at all",
			slo: linked("checkout-availability", ratio("good", "", "total"),
				v1.SLOObjective{}),
			want: ErrTarget,
		},
		{
			name: "a threshold objective with no operator",
			slo: linked("checkout-latency", threshold("latency"),
				v1.SLOObjective{Value: floatOf(0.5), Target: floatOf(0.99)}),
			want: ErrObjectiveValue,
		},
		{
			name: "a threshold objective with no value to compare against",
			slo: linked("checkout-latency", threshold("latency"),
				v1.SLOObjective{Operator: v1.OperatorLT, Target: floatOf(0.99)}),
			want: ErrObjectiveValue,
		},
		{
			name: "an indicator carrying neither metric",
			slo:  linked("checkout-availability", v1.SLISpec{}, v1.SLOObjective{Target: floatOf(0.99)}),
			want: ErrIndicator,
		},
		{
			name: "an SLI with no query under the key this target reads",
			slo: linked("checkout-availability",
				v1.SLISpec{ThresholdMetric: &v1.SLIMetricSpec{
					MetricSource: v1.SLIMetricSource{Type: SourceType, Spec: map[string]any{}},
				}},
				v1.SLOObjective{Operator: v1.OperatorLT, Value: floatOf(0.5)}),
			want: ErrQuery,
		},
		{
			name: "an SLI measured on something that does not speak PromQL",
			slo: linked("checkout-availability",
				v1.SLISpec{ThresholdMetric: &v1.SLIMetricSpec{
					MetricSource: v1.SLIMetricSource{Type: "ClickHouse", Spec: map[string]any{queryKey: "SELECT 1"}},
				}},
				v1.SLOObjective{Operator: v1.OperatorLT, Value: floatOf(0.5)}),
			want: ErrSourceType,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BurnQuery(detector(tt.slo))
			if err == nil {
				t.Fatal("BurnQuery succeeded, want an error")
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			// The author is looking at a directory, so the detector and the
			// SLO it linked to both have to be in the sentence.
			if !errors.Is(err, ErrUnlinked) {
				mentions(t, err.Error(), "checkout-request-rate", tt.slo.Metadata.Name)
			}
		})
	}
}

func mentions(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, text := range want {
		if !strings.Contains(got, text) {
			t.Errorf("error %q does not mention %q", got, text)
		}
	}
}

// TestBurnQueryRefusesAnotherDatasource is the SLO that lives on a different
// server. A backtest has one client and one window, so intersecting a firing
// timeline from one Prometheus with a budget from another would be two
// different months of history compared as if they were one.
func TestBurnQueryRefusesAnotherDatasource(t *testing.T) {
	d := detector(linked("checkout-availability",
		v1.SLISpec{ThresholdMetric: &v1.SLIMetricSpec{
			MetricSource: v1.SLIMetricSource{
				MetricSourceRef: "mimir-staging",
				Spec:            map[string]any{queryKey: "latency"},
			},
		}},
		v1.SLOObjective{Operator: v1.OperatorLT, Value: floatOf(0.5)}))

	// The detector reads the production Mimir by reference, which is the
	// shape that makes the two comparable at all.
	source, err := d.MetricSource()
	if err != nil {
		t.Fatalf("metricSource: %v", err)
	}
	source.Type = ""
	source.MetricSourceRef = "mimir-prod"
	d.Spec.Indicator.Spec.ThresholdMetric.MetricSource = source

	_, err = BurnQuery(d)
	if !errors.Is(err, ErrSourceMismatch) {
		t.Fatalf("got %v, want %v", err, ErrSourceMismatch)
	}
	mentions(t, err.Error(), "mimir-staging", "mimir-prod", "checkout-availability")
}
