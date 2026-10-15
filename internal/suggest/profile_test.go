package suggest

import (
	"math"
	"testing"
	"time"

	"github.com/dennisme/feno/internal/prometheus"
)

var profileStart = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// grid builds a series whose samples sit on the instants the reader returns,
// from a function over the index. A function returning a NaN at an index
// stands for a sample the server returned as non-finite, and one returning
// an infinity for a gap, since a gap is a point the reader never carries.
func grid(labels map[string]string, count int, step time.Duration, at func(i int) float64) prometheus.Series {
	series := prometheus.Series{Key: prometheus.SeriesKey(labels), Labels: labels}
	for i := range count {
		value := at(i)
		if math.IsInf(value, 1) {
			continue
		}
		series.Points = append(series.Points, prometheus.Point{
			At:    profileStart.Add(time.Duration(i) * step),
			Value: value,
		})
	}
	return series
}

func TestSummariseStatistics(t *testing.T) {
	step := time.Minute
	values := []float64{1, 2, 3, 4, 5}
	series := grid(map[string]string{"job": "one"}, len(values), step, func(i int) float64 { return values[i] })

	profile := summarise("up", []prometheus.Series{series},
		profileStart, profileStart.Add(4*step), step)

	if profile.Instants != 5 {
		t.Errorf("instants = %d, want 5", profile.Instants)
	}
	if len(profile.Series) != 1 {
		t.Fatalf("series = %d, want 1", len(profile.Series))
	}
	got := profile.Series[0]

	// Worked out by hand over 1..5: a median of 3, a population standard
	// deviation of the square root of 2, and an interquartile range of 2
	// rescaled by internal/detect's IQRToSigma.
	for _, check := range []struct {
		name string
		got  float64
		want float64
	}{
		{"median", got.Median, 3},
		{"standard deviation", got.StdDev, math.Sqrt2},
		{"robust scale", got.RobustScale, 2 / 1.349},
	} {
		if math.Abs(check.got-check.want) > 1e-9 {
			t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
		}
	}
	if got.Samples != 5 || got.NonFinite != 0 {
		t.Errorf("samples = %d, non-finite = %d, want 5 and 0", got.Samples, got.NonFinite)
	}
	if coverage := got.Coverage(profile.Instants); coverage != 1 {
		t.Errorf("coverage = %v, want 1", coverage)
	}
}

func TestSummariseCountsGapsAndNonFiniteApart(t *testing.T) {
	step := time.Minute
	// Index 1 and 3 are gaps, index 2 is a NaN the server returned.
	series := grid(map[string]string{"job": "one"}, 5, step, func(i int) float64 {
		switch i {
		case 1, 3:
			return math.Inf(1)
		case 2:
			return math.NaN()
		default:
			return 10
		}
	})

	profile := summarise("up", []prometheus.Series{series},
		profileStart, profileStart.Add(4*step), step)
	got := profile.Series[0]

	if got.Samples != 2 {
		t.Errorf("samples = %d, want 2: the two finite points", got.Samples)
	}
	if got.NonFinite != 1 {
		t.Errorf("non-finite = %d, want 1", got.NonFinite)
	}
	if coverage := got.Coverage(profile.Instants); math.Abs(coverage-0.4) > 1e-9 {
		t.Errorf("coverage = %v, want 0.4", coverage)
	}
	// The NaN must not reach the statistics, or every series holding one
	// would profile as undefined rather than as what it did the rest of the
	// time.
	if got.Median != 10 {
		t.Errorf("median = %v, want 10", got.Median)
	}
}

func TestSummariseAutocorrelation(t *testing.T) {
	step := time.Hour
	const days = 6
	count := days * 24
	series := grid(map[string]string{"job": "one"}, count, step, func(i int) float64 {
		return 100 + 10*math.Sin(2*math.Pi*float64(i)/24)
	})

	profile := summarise("up", []prometheus.Series{series},
		profileStart, profileStart.Add(time.Duration(count-1)*step), step)
	got := profile.Series[0]

	daily, ok := got.Autocorrelation[24*time.Hour]
	if !ok {
		t.Fatalf("no correlation measured at a day over %d days", days)
	}
	if daily < 0.99 {
		t.Errorf("daily correlation = %v over a pure daily sine, want above 0.99", daily)
	}
	// Six days cannot hold a week of overlap, and an unmeasured lag is
	// absent rather than zero so that a decision can say which it was.
	if correlation, ok := got.Autocorrelation[7*24*time.Hour]; ok {
		t.Errorf("weekly correlation = %v over %d days, want it unmeasured", correlation, days)
	}
}

func TestSummariseFlatSeriesHasNoCorrelation(t *testing.T) {
	step := time.Hour
	count := 6 * 24
	series := grid(map[string]string{"job": "one"}, count, step, func(int) float64 { return 100 })

	profile := summarise("up", []prometheus.Series{series},
		profileStart, profileStart.Add(time.Duration(count-1)*step), step)

	// A constant series has no variance, so a correlation over it is a
	// division by zero rather than a one. It has to come back as no shape,
	// since the alternative is a seasonal band over a flat line.
	if correlation := profile.Series[0].Autocorrelation[24*time.Hour]; correlation != 0 {
		t.Errorf("daily correlation = %v over a constant series, want 0", correlation)
	}
}
