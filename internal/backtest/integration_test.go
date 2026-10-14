//go:build integration

package backtest

import (
	"context"
	"os"
	"testing"
	"time"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"

	"github.com/dennisme/feno/internal/prometheus"
	"github.com/dennisme/feno/internal/spec"
)

// TestRunAgainstPrometheus is one of the two tests in this package that talk
// to a server.
//
// Everything else here is a pure function over the points a reader returned,
// so it runs against fixtures. This one exists because no fixture can prove
// the thing a backtest is finally judged on: that a real
// /api/v1/query_range, chunked across a range longer than the server's
// resolution limit and stitched back together, lands its samples on the same
// grid internal/detect matches instants against. A fixture cannot be wrong
// about that, which is exactly why it cannot be evidence for it either. If the
// grid were off by a millisecond every instant would come back absent, and
// that is what this checks.
//
// What it needs, and skips without:
//
//	FENO_PROMETHEUS_URL     base URL of a Prometheus, Mimir, Thanos or
//	                        VictoriaMetrics, such as http://localhost:9090
//	FENO_PROMETHEUS_QUERY   a PromQL query with data for the last month on
//	                        that server. Defaults to `up`, which every
//	                        Prometheus has and which is cheap to read.
//
//	go test -tags integration ./internal/backtest/
//
// It asserts only what is true of any server and any query, because what is
// under test is the plumbing and not somebody else's monitoring: the window
// reported is the window asked for, the samples landed on the grid, and a
// range past the resolution limit came back as one series per label set.
// Nothing here asserts a number of episodes.
func TestRunAgainstPrometheus(t *testing.T) {
	url := os.Getenv("FENO_PROMETHEUS_URL")
	if url == "" {
		t.Skip("set FENO_PROMETHEUS_URL to the base URL of a Prometheus-compatible server")
	}
	query := os.Getenv("FENO_PROMETHEUS_QUERY")
	if query == "" {
		query = "up"
	}

	detector := integrationDetector(query)

	client, err := prometheus.New(url)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	to := time.Now().UTC().Truncate(time.Minute)

	tests := []struct {
		name     string
		options  Options
		requests int
	}{
		{
			// A day reported with daily seasonality is four days read, which
			// at a one minute step is 5761 intervals: inside the limit, so one
			// request.
			name:     "a read inside the resolution limit",
			options:  Options{From: to.Add(-24 * time.Hour), To: to, Step: time.Minute},
			requests: 1,
		},
		{
			// Thirty days reported is thirty-three days read, 47521 intervals
			// against a limit of 11000, so five requests stitched into one
			// series per label set.
			name:     "a read past the resolution limit",
			options:  Options{From: to.Add(-30 * 24 * time.Hour), To: to, Step: time.Minute},
			requests: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := Run(ctx, client, detector, tt.options)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if !report.Window.Start.Equal(tt.options.From) || !report.Window.End.Equal(tt.options.To) {
				t.Errorf("reported window %s..%s, asked for %s..%s",
					instant(report.Window.Start), instant(report.Window.End),
					instant(tt.options.From), instant(tt.options.To))
			}
			if report.Stats.Requests < tt.requests {
				t.Errorf("the read took %d requests, want at least %d",
					report.Stats.Requests, tt.requests)
			}
			for _, warning := range report.Stats.Warnings {
				// Not a failure: a warning is the server's business. It is
				// logged because a trimmed read changes what every number
				// means, and a silent one would make this pass meaningless.
				t.Logf("server warning: %s", warning)
			}
			if len(report.Series) == 0 {
				t.Fatalf("query %q returned no series over %s on this server",
					query, duration(report.Window.Read()))
			}

			// The grid. A read that did not land on the instants being
			// evaluated produces a report of nothing but absence, which is
			// the symptom of every off-by-one in the chunk planner.
			var evaluated int
			for _, series := range report.Series {
				evaluated += series.Counts.Evaluated
			}
			if evaluated == 0 {
				t.Errorf("not one of %d series carried an evaluated instant: %+v",
					len(report.Series), report.Totals())
			}
		})
	}
}

// integrationDetector is the detector every case here replays: the simplest
// band that is not flat, over whatever query the environment named.
func integrationDetector(query string) spec.Detector {
	return spec.Detector{AnomalyDetector: spec.AnomalyDetector{
		APIVersion: spec.APIVersion,
		Kind:       spec.Kind,
		Metadata:   v1.Metadata{Name: "integration"},
		Spec: spec.DetectorSpec{
			Indicator: v1.SLOIndicatorInline{
				Metadata: v1.Metadata{Name: "integration"},
				Spec: v1.SLISpec{ThresholdMetric: &v1.SLIMetricSpec{
					MetricSource: v1.SLIMetricSource{
						Type: "Prometheus",
						Spec: map[string]any{"query": query},
					},
				}},
			},
			Detection: spec.Detection{
				Strategy:    "adaptive",
				Sensitivity: "balanced",
				Seasonality: []spec.Seasonality{spec.Daily},
				Class:       spec.ClassRequests,
				Window:      v1.NewDurationShorthand(1, "h"),
			},
			Alerting: spec.Alerting{For: v1.NewDurationShorthand(10, "m")},
		},
	}}
}

// TestBudgetOverlapAgainstPrometheus is the budget overlap against a real
// server, and the only evidence that the two range reads it intersects land
// on the same instants.
//
// No fixture can be that evidence. The overlap is an intersection of a firing
// timeline with a burn timeline, and two reads whose grids are offset from
// each other intersect in nothing however much the two conditions coincided,
// which is indistinguishable from the most valuable finding this report
// produces. So what is asserted is the grid and not a number of episodes: the
// burn signal answered at every reported instant, and it was burning at some
// of them.
//
// What it needs, and skips without, on top of the variables above:
//
//	FENO_SLO_GOOD_QUERY   the good half of an SLO's ratio over the same
//	                      server, such as sum(feno_good_per_second)
//	FENO_SLO_TOTAL_QUERY  the total half, such as
//	                      sum(feno_requests_per_second)
//
// `just backtest-history` passes all four against the generated month.
func TestBudgetOverlapAgainstPrometheus(t *testing.T) {
	url := os.Getenv("FENO_PROMETHEUS_URL")
	if url == "" {
		t.Skip("set FENO_PROMETHEUS_URL to the base URL of a Prometheus-compatible server")
	}
	good, total := os.Getenv("FENO_SLO_GOOD_QUERY"), os.Getenv("FENO_SLO_TOTAL_QUERY")
	if good == "" || total == "" {
		t.Skip("set FENO_SLO_GOOD_QUERY and FENO_SLO_TOTAL_QUERY to the two halves of an SLO ratio")
	}
	query := os.Getenv("FENO_PROMETHEUS_QUERY")
	if query == "" {
		query = "up"
	}

	detector := integrationDetector(query)
	detector.Spec.SLORef = "integration-availability"
	detector.SLO = &spec.SLO{
		SLO: v1.SLO{
			Metadata: v1.Metadata{Name: "integration-availability"},
			Spec: v1.SLOSpec{
				Service:         "integration",
				BudgetingMethod: v1.SLOBudgetingMethodOccurrences,
				Objectives:      []v1.SLOObjective{{Target: ratioTarget(0.999)}},
			},
		},
		Indicator: v1.SLISpec{RatioMetric: &v1.SLIRatioMetric{
			Counter: true,
			Good:    &v1.SLIMetricSpec{MetricSource: promSource(good)},
			Total:   &v1.SLIMetricSpec{MetricSource: promSource(total)},
		}},
	}

	client, err := prometheus.New(url)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// A week, because the seeded budget burns for one hour every third day
	// and a window that holds no burn would assert nothing.
	to := time.Now().UTC().Truncate(time.Minute)
	report, err := Run(ctx, client, detector, Options{
		From: to.Add(-7 * 24 * time.Hour),
		To:   to,
		Step: time.Minute,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.Budget == nil {
		t.Fatal("a detector linked to an SLO reported no budget")
	}
	counts := report.Budget.Counts
	t.Logf("burn signal %s %s: %+v", report.Budget.Query, report.Budget.Comparison(), counts)

	// The grid. Every reported instant has to carry a burn value, because the
	// read asked for exactly those instants at exactly that step. An absence
	// here is the off-by-one that would silently report every detector as
	// having coincided with nothing.
	if counts.Absent != 0 {
		t.Errorf("%d of %d reported instants carried no burn value",
			counts.Absent, report.Window.Instants())
	}
	if counts.Evaluated != report.Window.Instants() {
		t.Errorf("the burn signal answered at %d of %d instants",
			counts.Evaluated, report.Window.Instants())
	}
	// And the budget has to have burned somewhere, or an overlap of zero
	// below would be the absence of incidents rather than a verdict.
	if counts.Burning == 0 {
		t.Error("the budget never burned over the window, so an overlap of zero proves nothing")
	}
	if !report.Budget.Measured() {
		t.Error("a burn signal read over every instant reports itself unmeasured")
	}

	touched, episodes := report.TouchedEpisodes()
	t.Logf("%d of %d paging episodes coincided, %s of %s paged wall clock",
		touched, episodes, duration(report.CoincidentTime()), duration(report.PagedTime()))
	if touched > episodes {
		t.Errorf("%d episodes coincided of %d that fired", touched, episodes)
	}
	if report.CoincidentTime() > report.PagedTime() {
		t.Errorf("coincident time %s is longer than the paged time %s",
			report.CoincidentTime(), report.PagedTime())
	}
}

// ratioTarget and promSource are the two shapes an SLO needs that go-sdk
// takes by pointer and by extension map.
func ratioTarget(f float64) *float64 { return &f }

func promSource(query string) v1.SLIMetricSource {
	return v1.SLIMetricSource{Type: "Prometheus", Spec: map[string]any{"query": query}}
}
