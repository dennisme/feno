package suggest

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/feno/internal/detect"
	"github.com/dennisme/feno/internal/spec"
)

// day and week are the two lags a seasonal decision is read at.
const (
	day  = 24 * time.Hour
	week = 7 * 24 * time.Hour
)

// profiled builds a profile holding one series with the given statistics,
// filled in so that every field a decision does not read is fit rather than
// absent. Each test then sets only what it is about.
func profiled(query string, series ...SeriesProfile) Profile {
	return Profile{
		Query:    query,
		From:     time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
		To:       time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		Step:     5 * time.Minute,
		Instants: 6048,
		Series:   series,
	}
}

// fit is a series nothing refuses: fully covered, roughly normal, flat.
func fit() SeriesProfile {
	return SeriesProfile{
		Key:             `{service="checkout"}`,
		Samples:         6048,
		Median:          100,
		StdDev:          10,
		RobustScale:     10,
		Autocorrelation: map[time.Duration]float64{day: 0.1, week: 0.1},
	}
}

func TestDecideClass(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  spec.Class
	}{
		{
			name:  "a request rate is requests",
			query: `sum(rate(http_requests_total{service="checkout"}[5m]))`,
			want:  spec.ClassRequests,
		},
		{
			name:  "a quantile over a bucket is latency",
			query: `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`,
			want:  spec.ClassLatency,
		},
		{
			name:  "an error count is errors",
			query: `sum(rate(http_errors_total{service="checkout"}[5m]))`,
			want:  spec.ClassErrors,
		},
		{
			name:  "a memory usage is saturation",
			query: `max(container_memory_working_set_bytes{service="checkout"})`,
			want:  spec.ClassSaturation,
		},
		{
			name:  "latency wins over the requests in its own name",
			query: `histogram_quantile(0.9, rate(http_request_latency_seconds_bucket[5m]))`,
			want:  spec.ClassLatency,
		},
		{
			name:  "a label value naming errors does not make a total an error count",
			query: `sum(rate(http_requests_total{service="checkout"}[5m]))`,
			want:  spec.ClassRequests,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := Decide(profiled(tc.query, fit()))
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if decision.Class != tc.want {
				t.Errorf("class = %q, want %q", decision.Class, tc.want)
			}
		})
	}
}

func TestDecideStrategy(t *testing.T) {
	tests := []struct {
		name        string
		stdDev      float64
		robustScale float64
		want        detect.Strategy
	}{
		{
			name:        "the two scales agreeing is adaptive",
			stdDev:      10,
			robustScale: 10,
			want:        detect.Adaptive,
		},
		{
			name:        "a standard deviation at the threshold is robust",
			stdDev:      15,
			robustScale: 10,
			want:        detect.Robust,
		},
		{
			name:        "a standard deviation under the threshold is adaptive",
			stdDev:      14,
			robustScale: 10,
			want:        detect.Adaptive,
		},
		{
			name:        "spikes far above the robust scale are robust",
			stdDev:      80,
			robustScale: 10,
			want:        detect.Robust,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			series := fit()
			series.StdDev, series.RobustScale = tc.stdDev, tc.robustScale
			decision, err := Decide(profiled("up", series))
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if decision.Strategy != tc.want {
				t.Errorf("strategy = %q, want %q", decision.Strategy, tc.want)
			}
		})
	}
}

func TestDecideSeasonality(t *testing.T) {
	tests := []struct {
		name string
		corr map[time.Duration]float64
		want []spec.Seasonality
	}{
		{
			name: "no correlation at either lag is no seasonality",
			corr: map[time.Duration]float64{day: 0.1, week: 0.2},
			want: nil,
		},
		{
			name: "a daily correlation alone is daily",
			corr: map[time.Duration]float64{day: 0.8, week: 0.2},
			want: []spec.Seasonality{spec.Daily},
		},
		{
			name: "a weekly correlation alone is weekly",
			corr: map[time.Duration]float64{day: 0.2, week: 0.7},
			want: []spec.Seasonality{spec.Weekly},
		},
		{
			name: "both correlated is both, in internal/spec's order",
			corr: map[time.Duration]float64{day: 0.8, week: 0.7},
			want: []spec.Seasonality{spec.Daily, spec.Weekly},
		},
		{
			name: "a lag the range was too short to measure is not seasonal",
			corr: map[time.Duration]float64{day: 0.8},
			want: []spec.Seasonality{spec.Daily},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			series := fit()
			series.Autocorrelation = tc.corr
			decision, err := Decide(profiled("up", series))
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if !slices.Equal(decision.Seasonality, tc.want) {
				t.Errorf("seasonality = %v, want %v", decision.Seasonality, tc.want)
			}
		})
	}
}

// TestDecideKeepsSeasonalityOverRobust is the one place two decisions
// conflict. A spiky series drafts `robust` and a repeating one drafts
// seasonality, and a band cannot be both: a median is not a function of the
// per-window medians, so [detect.Spec.Validate] refuses the combination and
// a drafted document holding it would fail `feno generate`.
//
// The measured shape wins, because the shape is a property of the series and
// the strategy is a choice about width. The evidence has to say so, since an
// author reading `adaptive` on a spiky series would otherwise think the
// profile had missed the spikes.
func TestDecideKeepsSeasonalityOverRobust(t *testing.T) {
	series := fit()
	series.StdDev, series.RobustScale = 80, 10
	series.Autocorrelation = map[time.Duration]float64{day: 0.8, week: 0.2}

	decision, err := Decide(profiled("up", series))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Strategy != detect.Adaptive {
		t.Errorf("strategy = %q, want %q", decision.Strategy, detect.Adaptive)
	}
	if !slices.Equal(decision.Seasonality, []spec.Seasonality{spec.Daily}) {
		t.Errorf("seasonality = %v, want daily", decision.Seasonality)
	}

	var said bool
	for _, evidence := range decision.Evidence {
		if strings.Contains(evidence, string(detect.Robust)) &&
			strings.Contains(evidence, "seasonal") {
			said = true
		}
	}
	if !said {
		t.Errorf("no evidence says why the spiky series is not robust: %v", decision.Evidence)
	}
}

func TestDecideSensitivityIsNotProfiled(t *testing.T) {
	decision, err := Decide(profiled("up", fit()))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Sensitivity != detect.Balanced {
		t.Errorf("sensitivity = %q, want %q", decision.Sensitivity, detect.Balanced)
	}
}

func TestDecideRefuses(t *testing.T) {
	sparse := fit()
	sparse.Samples = 3000

	spiky := fit()
	spiky.RobustScale = 120
	spiky.StdDev = 130

	constant := fit()
	constant.StdDev, constant.RobustScale = 0, 0

	undefined := fit()
	undefined.NonFinite = 400

	tests := []struct {
		name    string
		profile Profile
		want    error
		reason  string
	}{
		{
			name:    "no series at all",
			profile: profiled("up"),
			want:    ErrNoSeries,
			reason:  "no series",
		},
		{
			name:    "a series too sparse to band",
			profile: profiled("up", sparse),
			want:    ErrUnfit,
			reason:  "too sparse",
		},
		{
			name:    "a scale as wide as the level",
			profile: profiled("up", spiky),
			want:    ErrUnfit,
			reason:  "too spiky",
		},
		{
			name:    "a constant series",
			profile: profiled("up", constant),
			want:    ErrUnfit,
			reason:  "constant",
		},
		{
			name:    "a series holding NaN or an infinity",
			profile: profiled("up", undefined),
			want:    ErrUnfit,
			reason:  "non-finite",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decide(tc.profile)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Decide error = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error %q does not say %q", err, tc.reason)
			}
		})
	}
}

func TestDecideRefusesOnlyWhenMostSeriesAreUnfit(t *testing.T) {
	sparse := fit()
	sparse.Key, sparse.Samples = `{service="cart"}`, 10

	if _, err := Decide(profiled("up", fit(), fit(), sparse)); err != nil {
		t.Errorf("one sparse series in three: Decide = %v, want a draft", err)
	}

	other := sparse
	other.Key = `{service="search"}`
	_, err := Decide(profiled("up", fit(), sparse, other))
	if !errors.Is(err, ErrUnfit) {
		t.Errorf("two sparse series in three: Decide = %v, want %v", err, ErrUnfit)
	}
}

func TestDecideEvidenceNamesEveryFieldItSet(t *testing.T) {
	series := fit()
	series.StdDev, series.RobustScale = 30, 10
	series.Autocorrelation = map[time.Duration]float64{day: 0.81, week: 0.12}

	decision, err := Decide(profiled(`sum(rate(http_requests_total[5m]))`, series))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	evidence := strings.Join(decision.Evidence, "\n")
	for _, want := range []string{"class requests", "strategy robust", "seasonality", "sensitivity", "coverage"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("evidence does not account for %q:\n%s", want, evidence)
		}
	}
	if !strings.Contains(evidence, "3") {
		t.Errorf("evidence does not carry the scale ratio behind the strategy:\n%s", evidence)
	}
	if !strings.Contains(evidence, "0.81") {
		t.Errorf("evidence does not carry the correlation behind the seasonality:\n%s", evidence)
	}
}
