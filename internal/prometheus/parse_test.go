package prometheus

import (
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return b
}

func TestParseResponseMatrix(t *testing.T) {
	series, warnings, err := parseResponse(http.StatusOK, fixture(t, "matrix.json"))
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("got warnings %v, want none", warnings)
	}
	if len(series) != 2 {
		t.Fatalf("got %d series, want 2", len(series))
	}

	first := series[0]
	if got := first.Labels["instance"]; got != "a:9100" {
		t.Errorf("instance label: got %q, want %q", got, "a:9100")
	}
	wantPoints := []Point{
		{At: at(0), Value: 1},
		{At: at(1), Value: 2.5},
		// A fractional timestamp is Prometheus reporting milliseconds as a
		// JSON float, not a point off the grid.
		{At: at(2).Add(500 * time.Millisecond), Value: 3},
	}
	if len(first.Points) != len(wantPoints) {
		t.Fatalf("got %d points, want %d", len(first.Points), len(wantPoints))
	}
	for i, want := range wantPoints {
		got := first.Points[i]
		if !got.At.Equal(want.At) || got.Value != want.Value {
			t.Errorf("point %d: got %s=%v, want %s=%v", i, got.At, got.Value, want.At, want.Value)
		}
	}

	// The sample is a string in the wire format precisely so these three can
	// be carried, and a backtest that dropped them would read a saturated
	// counter as a quiet hour.
	second := series[1].Points
	if !math.IsNaN(second[0].Value) {
		t.Errorf("got %v, want NaN", second[0].Value)
	}
	if !math.IsInf(second[1].Value, 1) {
		t.Errorf("got %v, want +Inf", second[1].Value)
	}
	if !math.IsInf(second[2].Value, -1) {
		t.Errorf("got %v, want -Inf", second[2].Value)
	}
}

func TestParseResponseEmpty(t *testing.T) {
	series, _, err := parseResponse(http.StatusOK, fixture(t, "matrix_empty.json"))
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if len(series) != 0 {
		t.Fatalf("got %d series, want none", len(series))
	}
}

func TestParseResponseWarnings(t *testing.T) {
	series, warnings, err := parseResponse(http.StatusOK, fixture(t, "matrix_warnings.json"))
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1", len(series))
	}
	want := []string{"results truncated due to limit"}
	if len(warnings) != 1 || warnings[0] != want[0] {
		t.Fatalf("got warnings %v, want %v", warnings, want)
	}
}

func TestParseResponseErrors(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     []byte
		want     error
		contains []string
	}{
		{
			name:   "a query error carries both of the server's fields",
			status: http.StatusBadRequest,
			body:   fixture(t, "error_bad_query.json"),
			want:   ErrQuery,
			// Either field alone leaves the reader guessing: the type says
			// whose fault it is and the message says what to change.
			contains: []string{"bad_data", "parse error"},
		},
		{
			name:     "a body from something that is not a Prometheus",
			status:   http.StatusGatewayTimeout,
			body:     fixture(t, "gateway_timeout.html"),
			want:     ErrResponse,
			contains: []string{"504", "Gateway Time-out"},
		},
		{
			name:     "an instant query answered where a range was asked for",
			status:   http.StatusOK,
			body:     []byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`),
			want:     ErrResponse,
			contains: []string{"vector", "matrix"},
		},
		{
			name:     "a sample that is not a number",
			status:   http.StatusOK,
			body:     []byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1790812800,"high"]]}]}}`),
			want:     ErrResponse,
			contains: []string{"high"},
		},
		{
			name:     "a value pair that is not a pair",
			status:   http.StatusOK,
			body:     []byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1790812800]]}]}}`),
			want:     ErrResponse,
			contains: []string{"sample"},
		},
		{
			name:     "an error status with no error fields",
			status:   http.StatusOK,
			body:     []byte(`{"status":"error"}`),
			want:     ErrQuery,
			contains: []string{"unknown"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseResponse(tc.status, tc.body)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got error %v, want %v", err, tc.want)
			}
			for _, want := range tc.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestSeriesKeyIsOrderIndependent(t *testing.T) {
	// Go randomises map iteration, so the same label set built twice is the
	// test: a key derived from iteration order would differ across enough
	// rounds.
	first := SeriesKey(map[string]string{
		"__name__": "http_requests_total",
		"job":      "api",
		"instance": "a:9100",
		"path":     "/v1/checkout",
	})
	for range 100 {
		again := SeriesKey(map[string]string{
			"path":     "/v1/checkout",
			"instance": "a:9100",
			"job":      "api",
			"__name__": "http_requests_total",
		})
		if again != first {
			t.Fatalf("key is not stable: %q then %q", first, again)
		}
	}
}

func TestSeriesKeyDistinguishesLabelSets(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
	}{
		{name: "empty", labels: map[string]string{}},
		{name: "one label", labels: map[string]string{"job": "api"}},
		{name: "a different value", labels: map[string]string{"job": "web"}},
		{name: "two labels", labels: map[string]string{"job": "api", "instance": "a:9100"}},
		// A separator in a label value is what a naive join collapses, and
		// two series reported as one is a report counting the wrong number of
		// instances.
		{name: "a value holding the separator", labels: map[string]string{"job": `api",instance="x`}},
		{name: "a value holding a quote", labels: map[string]string{"job": `api"`}},
		{name: "the same text split differently", labels: map[string]string{`job",x`: "api"}},
	}

	seen := make(map[string]string, len(tests))
	for _, tc := range tests {
		key := SeriesKey(tc.labels)
		if prev, dup := seen[key]; dup {
			t.Errorf("%s collides with %s on key %q", tc.name, prev, key)
			continue
		}
		seen[key] = tc.name
	}
}
