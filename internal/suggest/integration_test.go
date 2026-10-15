//go:build integration

package suggest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dennisme/feno/internal/prometheus"
)

// TestMeasureAgainstPrometheus is the only test here that talks to a server.
//
// Everything else in this package is a pure function over a profile or over
// a list of points, so it runs against fixtures. This one exists because no
// fixture can prove what a profile is finally judged on: that the samples a
// real /api/v1/query_range returns land on the grid [summariseSeries] lays
// them back onto. If the grid were off by one step, every instant would come
// back as a gap, every series would profile as too sparse to band, and the
// command would refuse a healthy repository with a measurement behind it. A
// fixture cannot be wrong about the grid, which is why it cannot be evidence
// for it either.
//
// What it needs, and skips without:
//
//	FENO_PROMETHEUS_URL     base URL of a Prometheus, Mimir, Thanos or
//	                        VictoriaMetrics, such as http://localhost:9090
//	FENO_PROMETHEUS_QUERY   a PromQL query with data over the last few days
//	                        on that server. Defaults to `up`, which every
//	                        Prometheus has and which is cheap to read.
//
//	go test -tags integration ./internal/suggest/
//
// It asserts only what is true of any server and any query, because what is
// under test is the plumbing and not somebody else's monitoring: the range
// reported is the range asked for, something came back, and the samples
// landed densely enough on the grid to be recognisable as the series that was
// read rather than as a gap. Nothing here asserts a strategy or a
// seasonality: those are decisions about the data, and the data belongs to
// whoever ran the test.
func TestMeasureAgainstPrometheus(t *testing.T) {
	url := os.Getenv("FENO_PROMETHEUS_URL")
	if url == "" {
		t.Skip("set FENO_PROMETHEUS_URL to the base URL of a Prometheus-compatible server")
	}
	query := os.Getenv("FENO_PROMETHEUS_QUERY")
	if query == "" {
		query = "up"
	}

	client, err := prometheus.New(url)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	step := 5 * time.Minute
	to := time.Now().UTC().Truncate(step)
	from := to.Add(-3 * 24 * time.Hour)

	profile, err := Measure(ctx, client, query, from, to, step)
	if err != nil {
		t.Fatalf("Measure over %q: %v", query, err)
	}

	if !profile.From.Equal(from) || !profile.To.Equal(to) {
		t.Errorf("profiled %s to %s, asked for %s to %s", profile.From, profile.To, from, to)
	}
	if profile.Instants != int(to.Sub(from)/step)+1 {
		t.Errorf("instants = %d over %s at %s", profile.Instants, to.Sub(from), step)
	}
	if len(profile.Series) == 0 {
		t.Fatalf("%q returned no series over the last three days; point "+
			"FENO_PROMETHEUS_QUERY at something this server holds", query)
	}

	for _, series := range profile.Series {
		if series.Samples == 0 {
			t.Errorf("%s: no sample landed on the grid, which is the off-by-one this test is for",
				series.Key)
			continue
		}
		// A server scraping at anything up to the step should fill nearly
		// every instant. A low number here is the symptom of a grid that
		// does not line up, so it is worth failing on rather than noting:
		// the decisions this feeds refuse a series under three quarters.
		if coverage := series.Coverage(profile.Instants); coverage < 0.5 {
			t.Errorf("%s: %.1f%% of %d instants held a sample, which is too few to be a real gap",
				series.Key, coverage*100, profile.Instants)
		}
		// A lag of a day has to be measurable over three days of history,
		// since a profile that cannot look for a daily shape over three days
		// would never find one over any range the command defaults to.
		if _, ok := series.Autocorrelation[24*time.Hour]; !ok {
			t.Errorf("%s: no correlation measured at a day over three days of history", series.Key)
		}
	}
}
