package detect

import (
	"errors"
	"math"
	"testing"
	"time"
)

const epsilon = 1e-9

func near(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > epsilon {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestQuantile(t *testing.T) {
	// Prometheus interpolates between the two values either side of
	// q*(n-1), so these are the values quantile_over_time would return for
	// the same range.
	tests := []struct {
		name   string
		q      float64
		sorted []float64
		want   float64
	}{
		{name: "empty is NaN", q: 0.5, sorted: nil, want: math.NaN()},
		{name: "one value is that value", q: 0.25, sorted: []float64{7}, want: 7},
		{name: "median of an odd count is the middle", q: 0.5, sorted: []float64{1, 2, 3}, want: 2},
		{name: "median of an even count interpolates", q: 0.5, sorted: []float64{1, 2, 3, 4}, want: 2.5},
		{name: "a quarter of four lands between the first two", q: 0.25, sorted: []float64{1, 2, 3, 4}, want: 1.75},
		{name: "three quarters of five is exact", q: 0.75, sorted: []float64{1, 2, 3, 4, 5}, want: 4},
		{name: "zero is the minimum", q: 0, sorted: []float64{1, 2, 3, 4}, want: 1},
		{name: "one is the maximum", q: 1, sorted: []float64{1, 2, 3, 4}, want: 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := quantile(tc.q, tc.sorted)
			if math.IsNaN(tc.want) {
				if !math.IsNaN(got) {
					t.Errorf("quantile = %v, want NaN", got)
				}
				return
			}
			near(t, got, tc.want, "quantile")
		})
	}
}

func TestBand(t *testing.T) {
	// Worked by hand over {1,2,3,4,5}. Mean 3, population variance
	// (4+1+0+1+4)/5 = 2. Median 3, absolute deviations {2,1,0,1,2} whose
	// median is 1. Quartiles 2 and 4, so an interquartile range of 2.
	pool := []float64{1, 2, 3, 4, 5}

	tests := []struct {
		name       string
		spec       Spec
		pool       []float64
		wantCentre float64
		wantScale  float64
	}{
		{
			name:       "adaptive is the mean and the population standard deviation",
			spec:       Spec{Strategy: Adaptive},
			pool:       pool,
			wantCentre: 3,
			wantScale:  math.Sqrt2,
		},
		{
			name:       "robust mad is the median and 1.4826 times the median deviation",
			spec:       Spec{Strategy: Robust, RobustScale: ScaleMAD},
			pool:       pool,
			wantCentre: 3,
			wantScale:  MADToSigma,
		},
		{
			name:       "robust iqr is the median and the interquartile range over 1.349",
			spec:       Spec{Strategy: Robust, RobustScale: ScaleIQR},
			pool:       pool,
			wantCentre: 3,
			wantScale:  2 / IQRToSigma,
		},
		{
			name:       "a flat pool has no spread at all",
			spec:       Spec{Strategy: Adaptive},
			pool:       []float64{5, 5, 5},
			wantCentre: 5,
			wantScale:  0,
		},
		{
			name:       "an unsorted pool gives the same robust answer",
			spec:       Spec{Strategy: Robust, RobustScale: ScaleMAD},
			pool:       []float64{4, 1, 5, 3, 2},
			wantCentre: 3,
			wantScale:  MADToSigma,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.spec.band(tc.pool)
			near(t, b.Centre, tc.wantCentre, "centre")
			near(t, b.Scale, tc.wantScale, "scale")
		})
	}
}

func TestBandEdges(t *testing.T) {
	tests := []struct {
		name      string
		band      Band
		wantUpper float64
		wantLower float64
		breaching map[float64]bool
	}{
		{
			name:      "the margin multiplies the scale",
			band:      Band{Centre: 3, Scale: 2, Margin: 3},
			wantUpper: 9,
			wantLower: -3,
			breaching: map[float64]bool{9: false, 9.5: true, -3: false, -3.5: true},
		},
		{
			name:      "the minimum width is a floor on a band that would be too tight",
			band:      Band{Centre: 5, Scale: 0, Margin: 3, MinWidth: 2},
			wantUpper: 6,
			wantLower: 4,
			breaching: map[float64]bool{5.5: false, 6.5: true, 3.5: true},
		},
		{
			name:      "a wide enough band ignores the minimum width",
			band:      Band{Centre: 5, Scale: 2, Margin: 3, MinWidth: 2},
			wantUpper: 11,
			wantLower: -1,
			breaching: map[float64]bool{10: false},
		},
		{
			name:      "a NaN value breaches nothing, the way a comparison drops it",
			band:      Band{Centre: 5, Scale: 1, Margin: 3},
			wantUpper: 8,
			wantLower: 2,
			breaching: map[float64]bool{math.NaN(): false},
		},
		{
			name:      "an infinity compares normally",
			band:      Band{Centre: 5, Scale: 1, Margin: 3},
			wantUpper: 8,
			wantLower: 2,
			breaching: map[float64]bool{math.Inf(1): true, math.Inf(-1): true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			near(t, tc.band.Upper(), tc.wantUpper, "upper")
			near(t, tc.band.Lower(), tc.wantLower, "lower")
			for v, want := range tc.breaching {
				if got := tc.band.Breaches(v); got != want {
					t.Errorf("Breaches(%v) = %v, want %v", v, got, want)
				}
			}
		})
	}
}

// TestScalesAgree is what the capability model rests on: a specification that
// asks for robust gets a band of the same width whichever robust scale the
// target can express. Both are calibrated against the standard deviation of a
// normal distribution, so on normal input all three strategies must land on
// the same number.
//
// The input is the normal distribution's own quantiles rather than a sampled
// one, so there is nothing random to flake and the expected value is 1 by
// construction rather than by this code's say-so.
func TestScalesAgree(t *testing.T) {
	const n = 1001
	pool := make([]float64, n)
	for i := range pool {
		p := (float64(i) + 0.5) / n
		pool[i] = math.Sqrt2 * math.Erfinv(2*p-1)
	}

	specs := map[string]Spec{
		"adaptive": {Strategy: Adaptive},
		"mad":      {Strategy: Robust, RobustScale: ScaleMAD},
		"iqr":      {Strategy: Robust, RobustScale: ScaleIQR},
	}

	for name, spec := range specs {
		t.Run(name, func(t *testing.T) {
			b := spec.band(pool)
			if math.Abs(b.Centre) > 0.01 {
				t.Errorf("centre = %v, want 0", b.Centre)
			}
			if math.Abs(b.Scale-1) > 0.01 {
				t.Errorf("scale = %v, want 1 within 1%%", b.Scale)
			}
		})
	}
}

func TestLookback(t *testing.T) {
	day := 24 * time.Hour

	tests := []struct {
		name string
		spec Spec
		want time.Duration
	}{
		{
			name: "no seasonality needs one window",
			spec: Spec{Window: 30 * day},
			want: 30 * day,
		},
		{
			// The case to think through: the band at an instant reads a
			// 30 day window ending a week earlier, so the first evaluable
			// point is 37 days after the history starts.
			name: "weekly seasonality on a thirty day window needs thirty seven days",
			spec: Spec{Window: 30 * day, Offsets: []time.Duration{7 * day}},
			want: 37 * day,
		},
		{
			name: "the furthest offset decides",
			spec: Spec{Window: time.Hour, Offsets: []time.Duration{day, 2 * day, 7 * day}},
			want: 7*day + time.Hour,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.Lookback(); got != tc.want {
				t.Errorf("Lookback = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequires(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want []Requirement
	}{
		{
			name: "adaptive with no seasonality requires nothing optional",
			spec: Spec{Strategy: Adaptive},
		},
		{
			name: "robust by interquartile range requires nothing optional",
			spec: Spec{Strategy: Robust, RobustScale: ScaleIQR},
		},
		{
			name: "robust by median absolute deviation requires it",
			spec: Spec{Strategy: Robust, RobustScale: ScaleMAD},
			want: []Requirement{RequireMAD},
		},
		{
			name: "seasonal offsets require seasonal decomposition",
			spec: Spec{Strategy: Adaptive, Offsets: []time.Duration{24 * time.Hour}},
			want: []Requirement{RequireSeasonalDecomposition},
		},
		{
			name: "both at once",
			spec: Spec{Strategy: Robust, RobustScale: ScaleMAD, Offsets: []time.Duration{24 * time.Hour}},
			want: []Requirement{RequireMAD, RequireSeasonalDecomposition},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.Requires()
			if len(got) != len(tc.want) {
				t.Fatalf("Requires = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Requires[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestValidate(t *testing.T) {
	valid := Spec{
		Strategy:    Adaptive,
		Sensitivity: Balanced,
		Window:      time.Hour,
		MinSamples:  10,
	}

	with := func(f func(*Spec)) Spec {
		s := valid
		f(&s)
		return s
	}

	tests := []struct {
		name string
		spec Spec
		want error
	}{
		{name: "a complete adaptive spec", spec: valid},
		{
			name: "a complete robust spec",
			spec: with(func(s *Spec) { s.Strategy = Robust; s.RobustScale = ScaleMAD }),
		},
		{
			name: "an unknown strategy",
			spec: with(func(s *Spec) { s.Strategy = "ewma" }),
			want: ErrStrategy,
		},
		{
			name: "robust without a scale",
			spec: with(func(s *Spec) { s.Strategy = Robust }),
			want: ErrScale,
		},
		{
			name: "an unknown robust scale",
			spec: with(func(s *Spec) { s.Strategy = Robust; s.RobustScale = "tukey" }),
			want: ErrScale,
		},
		{
			name: "a scale on an adaptive spec means the author expected robust",
			spec: with(func(s *Spec) { s.RobustScale = ScaleMAD }),
			want: ErrScale,
		},
		{
			name: "an unknown sensitivity",
			spec: with(func(s *Spec) { s.Sensitivity = "paranoid" }),
			want: ErrSensitivity,
		},
		{
			name: "a zero window",
			spec: with(func(s *Spec) { s.Window = 0 }),
			want: ErrWindow,
		},
		{
			name: "a negative offset",
			spec: with(func(s *Spec) { s.Offsets = []time.Duration{-time.Hour} }),
			want: ErrOffset,
		},
		{
			name: "a repeated offset would weigh the same history twice",
			spec: with(func(s *Spec) { s.Offsets = []time.Duration{time.Hour, time.Hour} }),
			want: ErrOffset,
		},
		{
			name: "a negative minimum width",
			spec: with(func(s *Spec) { s.MinWidth = -1 }),
			want: ErrMinWidth,
		},
		{
			name: "no minimum sample count at all",
			spec: with(func(s *Spec) { s.MinSamples = 0 }),
			want: ErrMinSamples,
		},
		{
			name: "fewer than two samples cannot give a scale",
			spec: with(func(s *Spec) { s.MinSamples = 1 }),
			want: ErrMinSamples,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if !errors.Is(err, tc.want) {
				t.Errorf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestMarginMatchesSensitivity keeps the exported margin and the band it
// computes from the same table.
func TestMarginMatchesSensitivity(t *testing.T) {
	for _, sensitivity := range []Sensitivity{Aggressive, Balanced, Conservative} {
		s := Spec{
			Strategy:    Adaptive,
			Sensitivity: sensitivity,
			Window:      time.Hour,
			MinSamples:  2,
		}
		band := s.band([]float64{1, 2, 3})
		if band.Margin != s.Margin() {
			t.Errorf("%s: Margin() is %v, band carries %v", sensitivity, s.Margin(), band.Margin)
		}
	}
	if got := (Spec{Sensitivity: "paranoid"}).Margin(); got != 0 {
		t.Errorf("unknown sensitivity: got %v, want 0", got)
	}
}
