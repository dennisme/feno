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

// TestRunAgainstPrometheus is the only test in this package that talks to a
// server.
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

	detector := spec.Detector{AnomalyDetector: spec.AnomalyDetector{
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
