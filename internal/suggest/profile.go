package suggest

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/dennisme/feno/internal/detect"
	"github.com/dennisme/feno/internal/prometheus"
)

// Measure reads the query once over the range and reduces what came back to
// a [Profile].
//
// One read, for the reason internal/prometheus gives for not fanning out: the
// target is somebody's production monitoring. A directory of twenty SLIs is
// twenty range queries at it as it is, and a profile that read each one twice
// to check itself would be a command nobody is allowed to run.
func Measure(
	ctx context.Context, client *prometheus.Client, query string, from, to time.Time, step time.Duration,
) (Profile, error) {
	result, err := client.QueryRange(ctx, query, from, to, step)
	if err != nil {
		return Profile{}, err
	}
	return summarise(query, result.Series, from, to, step), nil
}

// summarise is the pure half of the read: the statistics, with no client in
// sight, so that what a decision is made from can be tested without a server.
func summarise(
	query string, series []prometheus.Series, from, to time.Time, step time.Duration,
) Profile {
	profile := Profile{
		Query:    query,
		From:     from,
		To:       to,
		Step:     step,
		Instants: instants(from, to, step),
	}
	for _, s := range series {
		profile.Series = append(profile.Series, summariseSeries(s, from, step, profile.Instants))
	}
	return profile
}

// instants is how many evaluation instants the range holds, which is what a
// coverage fraction is taken against.
//
// A range query is evaluated at the start instant and then every step up to
// and including the end, so a range of exactly one step holds two of them.
// This is deliberately not internal/detect's left-open window: that is the
// band's own boundary and this is a count of what the reader asked the server
// for.
func instants(from, to time.Time, step time.Duration) int {
	if step <= 0 || !to.After(from) {
		return 0
	}
	return int(to.Sub(from)/step) + 1
}

// summariseSeries is one label set's statistics.
//
// The samples are laid back onto the grid of instants first. The grid is what
// makes a gap distinguishable from a non-finite sample, which the reader's
// own point list cannot be: a gap is a point that is not there, so counting
// points alone says nothing about how much of the range was covered. It is
// also what a lag is indexed by, since a correlation at a day needs to know
// which sample is a day after which.
func summariseSeries(s prometheus.Series, from time.Time, step time.Duration, count int) SeriesProfile {
	var (
		present = make([]bool, count)
		values  = make([]float64, count)
		out     = SeriesProfile{Key: s.Key}
		finite  []float64
	)
	for _, point := range s.Points {
		index := int(math.Round(float64(point.At.Sub(from)) / float64(step)))
		if index < 0 || index >= count {
			continue
		}
		if math.IsNaN(point.Value) || math.IsInf(point.Value, 0) {
			out.NonFinite++
			continue
		}
		present[index] = true
		values[index] = point.Value
		finite = append(finite, point.Value)
	}

	out.Samples = len(finite)
	if out.Samples == 0 {
		return out
	}

	sorted := slices.Sorted(slices.Values(finite))
	out.Median = detect.Quantile(0.5, sorted)
	out.StdDev = detect.StdDev(finite)
	out.RobustScale = (detect.Quantile(0.75, sorted) - detect.Quantile(0.25, sorted)) / detect.IQRToSigma

	out.Autocorrelation = correlations(present, values, step)
	return out
}

// correlations is the correlation of the series with itself at each lag that
// the range was long enough to measure, keyed by lag.
func correlations(present []bool, values []float64, step time.Duration) map[time.Duration]float64 {
	out := map[time.Duration]float64{}
	for _, candidate := range lags {
		if correlation, ok := correlation(present, values, step, candidate.lag); ok {
			out[candidate.lag] = correlation
		}
	}
	return out
}

// correlation is Pearson's between the series and itself shifted by the lag,
// over the instants where both ends held a sample.
//
// It reports whether it could be measured at all, which is the answer that
// matters as much as the number. Two things make it unmeasurable, and they
// are different problems with the same shape. A range shorter than twice the
// lag has less than one period of overlap, so whatever correlation it finds
// is a statement about a single repetition. A series with no variance at
// either end divides by zero: a flat line correlates with nothing, including
// itself, and reporting a one there would put a seasonal band over a
// constant.
func correlation(present []bool, values []float64, step time.Duration, lag time.Duration) (float64, bool) {
	offset := int(math.Round(float64(lag) / float64(step)))
	if offset < 1 || offset >= len(present) {
		return 0, false
	}

	var left, right []float64
	for i := 0; i+offset < len(present); i++ {
		if !present[i] || !present[i+offset] {
			continue
		}
		left = append(left, values[i])
		right = append(right, values[i+offset])
	}
	// One period of overlap is the floor. Below it the pairs do not span the
	// shape being looked for.
	if time.Duration(len(left))*step < lag {
		return 0, false
	}

	leftMean, rightMean := mean(left), mean(right)
	var covariance, leftVariance, rightVariance float64
	for i := range left {
		a, b := left[i]-leftMean, right[i]-rightMean
		covariance += a * b
		leftVariance += a * a
		rightVariance += b * b
	}
	if leftVariance == 0 || rightVariance == 0 {
		return 0, true
	}
	return covariance / math.Sqrt(leftVariance*rightVariance), true
}

// mean is only ever used to centre a correlation. Nothing about a band is
// taken from it, so it does not need internal/detect's convention: there is
// only one way to average.
func mean(values []float64) float64 {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}
