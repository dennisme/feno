package spec

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"

	"github.com/dennisme/feno/internal/detect"
)

const (
	day  = 24 * time.Hour
	week = 7 * day
)

// TestLoadedDetectorBecomesABand is the end of the slice: a detector YAML in
// a directory turns into the band internal/detect evaluates, with nothing
// between the document and [detect.Spec].
func TestLoadedDetectorBecomesABand(t *testing.T) {
	bundle, err := Load(filepath.Join("testdata", "mixed"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	byName := map[string]Detector{}
	for _, d := range bundle.Detectors {
		byName[d.Metadata.Name] = d
	}

	tests := []struct {
		name string
		step time.Duration
		want detect.Spec
	}{
		{
			// Daily and weekly together, which is six offset windows, and
			// the default coverage over an hour at a minute step: three
			// quarters of sixty samples, which each of the six windows
			// clears on its own.
			name: "checkout-request-rate",
			step: time.Minute,
			want: detect.Spec{
				Strategy:    detect.Adaptive,
				Sensitivity: detect.Conservative,
				Window:      time.Hour,
				Offsets:     []time.Duration{day, 2 * day, 3 * day, week, 2 * week, 3 * week},
				MinWidth:    0.5,
				MinSamples:  45,
			},
		},
		{
			// No seasonality, so one window ending at the instant, and an
			// authored coverage of half of thirty samples.
			name: "queue-depth",
			step: time.Minute,
			want: detect.Spec{
				Strategy:    detect.Adaptive,
				Sensitivity: detect.Balanced,
				Window:      30 * time.Minute,
				MinSamples:  15,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, ok := byName[tt.name]
			if !ok {
				t.Fatalf("detector %q not loaded", tt.name)
			}
			got, err := d.Band(tt.step)
			if err != nil {
				t.Fatalf("Band: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Band:\n got %+v\nwant %+v", got, tt.want)
			}
			// A band that internal/detect would reject is a band this
			// package should never have returned.
			if err := got.Validate(); err != nil {
				t.Errorf("detect rejected the band: %v", err)
			}
		})
	}
}

// TestLookbackFollowsSeasonality pins the consequence a backtest has to act
// on: three weeks of offsets plus the window, not three weeks and not one.
func TestLookbackFollowsSeasonality(t *testing.T) {
	bundle, err := Load(filepath.Join("testdata", "mixed"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, d := range bundle.Detectors {
		if d.Metadata.Name != "checkout-request-rate" {
			continue
		}
		band, err := d.Band(time.Minute)
		if err != nil {
			t.Fatalf("Band: %v", err)
		}
		if want := 3*week + time.Hour; band.Lookback() != want {
			t.Errorf("Lookback: got %v, want %v", band.Lookback(), want)
		}
		return
	}
	t.Fatal("detector checkout-request-rate not loaded")
}

func TestSeasonalOffsets(t *testing.T) {
	tests := []struct {
		name  string
		names []Seasonality
		want  []time.Duration
	}{
		{
			name:  "none means one window ending at the instant",
			names: nil,
			want:  nil,
		},
		{
			name:  "daily",
			names: []Seasonality{Daily},
			want:  []time.Duration{day, 2 * day, 3 * day},
		},
		{
			name:  "weekly",
			names: []Seasonality{Weekly},
			want:  []time.Duration{week, 2 * week, 3 * week},
		},
		{
			// Authoring order must not change the band, or a diff that only
			// reorders a list changes the generated rules.
			name:  "weekly before daily is the same band",
			names: []Seasonality{Weekly, Daily},
			want:  []time.Duration{day, 2 * day, 3 * day, week, 2 * week, 3 * week},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := seasonalOffsets(tt.names); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestOffsetsAreDistinct is why daily and weekly can be read together at all:
// internal/detect rejects a repeated offset, and a week is not a multiple of
// any of the first three days.
func TestOffsetsAreDistinct(t *testing.T) {
	offsets := seasonalOffsets([]Seasonality{Daily, Weekly})
	seen := map[time.Duration]bool{}
	for _, d := range offsets {
		if seen[d] {
			t.Errorf("offset %v appears twice in %v", d, offsets)
		}
		seen[d] = true
	}
}

func TestBandErrors(t *testing.T) {
	// base is a detector that converts, so each case below changes exactly
	// one thing.
	base := func() AnomalyDetector {
		return AnomalyDetector{
			APIVersion: APIVersion,
			Kind:       Kind,
			Metadata:   metadata("checkout-request-rate"),
			Spec: DetectorSpec{
				Indicator: indicator("Prometheus", "", "up"),
				Detection: Detection{
					Strategy:    detect.Adaptive,
					Sensitivity: detect.Balanced,
					Class:       ClassRequests,
				},
			},
		}
	}

	tests := []struct {
		name   string
		step   time.Duration
		mutate func(*AnomalyDetector)
		want   error
	}{
		{
			name: "no step",
			step: 0,
			want: ErrStep,
		},
		{
			// A window shorter than the step holds nothing, so there is no
			// coverage fraction that rescues it.
			name:   "window shorter than the step",
			step:   5 * time.Minute,
			mutate: func(d *AnomalyDetector) { d.Spec.Detection.Window = shorthand(t, "1m") },
			want:   ErrCoverage,
		},
		{
			// A zero window stringifies to empty whatever unit it carries,
			// so this is the case that must not quietly become the default.
			name: "a window of zero",
			step: time.Minute,
			mutate: func(d *AnomalyDetector) {
				d.Spec.Detection.Window = v1.NewDurationShorthand(0, v1.DurationShorthandUnitHour)
			},
			want: ErrWindow,
		},
		{
			name:   "a window with a unit that is not one",
			step:   time.Minute,
			mutate: func(d *AnomalyDetector) { d.Spec.Detection.Window = shorthand(t, "5x") },
			want:   ErrWindow,
		},
		{
			name:   "unknown strategy",
			step:   time.Minute,
			mutate: func(d *AnomalyDetector) { d.Spec.Detection.Strategy = "ewma" },
			want:   detect.ErrStrategy,
		},
		{
			name:   "unknown sensitivity",
			step:   time.Minute,
			mutate: func(d *AnomalyDetector) { d.Spec.Detection.Sensitivity = "paranoid" },
			want:   detect.ErrSensitivity,
		},
		{
			// internal/detect rejects a robust scale on the adaptive
			// strategy, and the default here must not paper over it.
			name:   "robust scale on the adaptive strategy",
			step:   time.Minute,
			mutate: func(d *AnomalyDetector) { d.Spec.Detection.RobustScale = detect.ScaleMAD },
			want:   detect.ErrScale,
		},
		{
			name:   "unknown class",
			step:   time.Minute,
			mutate: func(d *AnomalyDetector) { d.Spec.Detection.Class = "throughput" },
			want:   ErrClass,
		},
		{
			name:   "unknown seasonality",
			step:   time.Minute,
			mutate: func(d *AnomalyDetector) { d.Spec.Detection.Seasonality = []Seasonality{"monthly"} },
			want:   ErrSeasonality,
		},
		{
			name: "repeated seasonality",
			step: time.Minute,
			mutate: func(d *AnomalyDetector) {
				d.Spec.Detection.Seasonality = []Seasonality{Daily, Daily}
			},
			want: ErrSeasonality,
		},
		{
			name:   "coverage above one",
			step:   time.Minute,
			mutate: func(d *AnomalyDetector) { d.Spec.Detection.MinCoverage = 1.5 },
			want:   ErrMinCoverage,
		},
		{
			name:   "no name",
			step:   time.Minute,
			mutate: func(d *AnomalyDetector) { d.Metadata.Name = "" },
			want:   ErrName,
		},
		{
			name:   "neither a type nor a metricSourceRef",
			step:   time.Minute,
			mutate: func(d *AnomalyDetector) { d.Spec.Indicator = indicator("", "", "up") },
			want:   ErrMetricSource,
		},
		{
			name:   "both a type and a metricSourceRef",
			step:   time.Minute,
			mutate: func(d *AnomalyDetector) { d.Spec.Indicator = indicator("Prometheus", "mimir-prod", "up") },
			want:   ErrMetricSource,
		},
		{
			name:   "no query",
			step:   time.Minute,
			mutate: func(d *AnomalyDetector) { d.Spec.Indicator = indicator("Prometheus", "", "") },
			want:   ErrQuery,
		},
		{
			name: "a ratio is more than one series",
			step: time.Minute,
			mutate: func(d *AnomalyDetector) {
				d.Spec.Indicator.Spec.ThresholdMetric = nil
				d.Spec.Indicator.Spec.RatioMetric = &v1.SLIRatioMetric{}
			},
			want: ErrIndicator,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := base()
			if tt.mutate != nil {
				tt.mutate(&d)
			}
			_, err := d.Band(tt.step)
			if !errors.Is(err, tt.want) {
				t.Errorf("got %v, want %v", err, tt.want)
			}
		})
	}
}

// TestRobustDefaultsToTheScaleEveryServerHas keeps the capability model
// honest: a document that says `robust` and nothing else must not require an
// experimental Prometheus function.
func TestRobustDefaultsToTheScaleEveryServerHas(t *testing.T) {
	d := AnomalyDetector{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   metadata("checkout-request-rate"),
		Spec: DetectorSpec{
			Indicator: indicator("Prometheus", "", "up"),
			Detection: Detection{
				Strategy:    detect.Robust,
				Sensitivity: detect.Balanced,
				Class:       ClassRequests,
			},
		},
	}
	band, err := d.Band(time.Minute)
	if err != nil {
		t.Fatalf("Band: %v", err)
	}
	if band.RobustScale != detect.ScaleIQR {
		t.Errorf("robust scale: got %q, want %q", band.RobustScale, detect.ScaleIQR)
	}
	for _, requirement := range band.Requires() {
		if requirement == detect.RequireMAD {
			t.Error("a bare `robust` band requires an experimental server feature")
		}
	}
}

// TestDefaultWindowClearsTheSampleFloor is the check behind the default: at
// every step a Prometheus deployment runs, an hour holds far more than the
// two samples a scale needs.
func TestDefaultWindowClearsTheSampleFloor(t *testing.T) {
	for _, step := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute} {
		d := AnomalyDetector{
			APIVersion: APIVersion,
			Kind:       Kind,
			Metadata:   metadata("checkout-request-rate"),
			Spec: DetectorSpec{
				Indicator: indicator("Prometheus", "", "up"),
				Detection: Detection{
					Strategy:    detect.Adaptive,
					Sensitivity: detect.Balanced,
					Class:       ClassRequests,
				},
			},
		}
		band, err := d.Band(step)
		if err != nil {
			t.Fatalf("step %v: Band: %v", step, err)
		}
		if band.Window != DefaultWindow {
			t.Errorf("step %v: window %v, want %v", step, band.Window, DefaultWindow)
		}
		if want := int(DefaultMinCoverage * float64(DefaultWindow/step)); band.MinSamples != want {
			t.Errorf("step %v: minimum samples %d, want %d", step, band.MinSamples, want)
		}
	}
}

// metadata is the least a detector needs to be reportable on.
func metadata(name string) v1.Metadata { return v1.Metadata{Name: name} }

// indicator builds the one shape a detector accepts: an inline OpenSLO
// indicator carrying a single thresholdMetric. An empty query leaves the
// metric source spec empty, which is the no-query case.
func indicator(sourceType, ref, query string) v1.SLOIndicatorInline {
	source := v1.SLIMetricSource{Type: sourceType, MetricSourceRef: ref}
	if query != "" {
		source.Spec = map[string]any{"query": query}
	}
	return v1.SLOIndicatorInline{
		Metadata: v1.Metadata{Name: "checkout-requests"},
		Spec:     v1.SLISpec{ThresholdMetric: &v1.SLIMetricSpec{MetricSource: source}},
	}
}

func shorthand(t *testing.T, s string) v1.DurationShorthand {
	t.Helper()
	d, err := v1.ParseDurationShorthand(s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return d
}
