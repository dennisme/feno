package prometheus

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const step = time.Minute

// recorder is an httptest handler that serves the given fixtures in order and
// keeps the form of every request it answered, which is how the chunk
// boundaries that went out are asserted rather than assumed.
type recorder struct {
	t        *testing.T
	status   int
	bodies   []string
	requests []url.Values
	headers  []http.Header
	paths    []string
}

func serve(t *testing.T, status int, fixtures ...string) (*Client, *recorder) {
	t.Helper()

	rec := &recorder{t: t, status: status, bodies: fixtures}
	srv := httptest.NewServer(http.HandlerFunc(rec.handle))
	t.Cleanup(srv.Close)

	client, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client.HTTP = srv.Client()
	return client, rec
}

func (r *recorder) handle(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseForm(); err != nil {
		r.t.Errorf("parsing request form: %v", err)
	}
	n := len(r.requests)
	r.requests = append(r.requests, req.PostForm)
	r.headers = append(r.headers, req.Header.Clone())
	r.paths = append(r.paths, req.URL.Path)

	if n >= len(r.bodies) {
		r.t.Errorf("request %d has no fixture to answer with", n)
		http.Error(w, "no fixture", http.StatusInternalServerError)
		return
	}

	name := r.bodies[n]
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(name, ".html") {
		w.Header().Set("Content-Type", "text/html")
	}
	w.WriteHeader(r.status)
	if _, err := w.Write(fixture(r.t, name)); err != nil {
		r.t.Errorf("writing response: %v", err)
	}
}

func TestQueryRangeSingleChunk(t *testing.T) {
	client, rec := serve(t, http.StatusOK, "matrix.json")

	result, err := client.QueryRange(context.Background(), `rate(http_requests_total[5m])`, at(0), at(2), step)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}

	if result.Stats.Requests != 1 {
		t.Errorf("got %d requests, want 1", result.Stats.Requests)
	}
	if result.Stats.Points != 6 {
		t.Errorf("got %d points, want 6", result.Stats.Points)
	}
	if len(result.Series) != 2 {
		t.Fatalf("got %d series, want 2", len(result.Series))
	}
	if result.Series[0].Key == result.Series[1].Key {
		t.Errorf("two label sets share the key %q", result.Series[0].Key)
	}

	form := rec.requests[0]
	want := url.Values{
		"query": {`rate(http_requests_total[5m])`},
		"start": {formatTime(at(0))},
		"end":   {formatTime(at(2))},
		"step":  {"60"},
	}
	for name, values := range want {
		if got := form[name]; len(got) != 1 || got[0] != values[0] {
			t.Errorf("request %s: got %v, want %v", name, got, values)
		}
	}
}

func TestQueryRangeStitchesChunks(t *testing.T) {
	client, rec := serve(t, http.StatusOK, "chunk_first.json", "chunk_second.json")
	// One interval per request, so a four point range needs two of them.
	client.MaxPointsPerSeries = 1

	result, err := client.QueryRange(context.Background(), "up", at(0), at(3), step)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}

	if result.Stats.Requests != 2 {
		t.Fatalf("got %d requests, want 2", result.Stats.Requests)
	}
	if len(result.Series) != 1 {
		t.Fatalf("got %d series, want 1: the same label set from two requests is one series",
			len(result.Series))
	}

	points := result.Series[0].Points
	wantValues := []float64{1, 2, 3, 4}
	if len(points) != len(wantValues) {
		t.Fatalf("got %d points, want %d", len(points), len(wantValues))
	}
	for i, want := range wantValues {
		if points[i].Value != want {
			t.Errorf("point %d: got %v, want %v", i, points[i].Value, want)
		}
		if !points[i].At.Equal(at(i)) {
			t.Errorf("point %d: got %s, want %s", i, points[i].At, at(i))
		}
	}

	// The requests themselves: a second chunk starting where the first ended
	// would have fetched at(1) twice, and the stitched series above would still
	// look plausible.
	if got, want := rec.requests[0]["end"][0], formatTime(at(1)); got != want {
		t.Errorf("first chunk end: got %s, want %s", got, want)
	}
	if got, want := rec.requests[1]["start"][0], formatTime(at(2)); got != want {
		t.Errorf("second chunk start: got %s, want %s", got, want)
	}
}

func TestQueryRangeEmptyResult(t *testing.T) {
	client, _ := serve(t, http.StatusOK, "matrix_empty.json")

	result, err := client.QueryRange(context.Background(), "up", at(0), at(2), step)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(result.Series) != 0 || result.Stats.Points != 0 {
		t.Fatalf("got %d series and %d points, want none", len(result.Series), result.Stats.Points)
	}
	if result.Stats.Requests != 1 {
		t.Errorf("got %d requests, want 1", result.Stats.Requests)
	}
}

func TestQueryRangeWarningsAreCollectedOnce(t *testing.T) {
	client, _ := serve(t, http.StatusOK, "matrix_warnings.json", "matrix_warnings.json")
	client.MaxPointsPerSeries = 1

	result, err := client.QueryRange(context.Background(), "up", at(0), at(3), step)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	// Every chunk of a trimmed query warns, and the report wants the fact once
	// rather than once per request.
	want := []string{"results truncated due to limit"}
	if len(result.Stats.Warnings) != 1 || result.Stats.Warnings[0] != want[0] {
		t.Fatalf("got warnings %v, want %v", result.Stats.Warnings, want)
	}
}

func TestQueryRangeServerError(t *testing.T) {
	client, _ := serve(t, http.StatusBadRequest, "error_bad_query.json")

	_, err := client.QueryRange(context.Background(), "{", at(0), at(2), step)
	if !errors.Is(err, ErrQuery) {
		t.Fatalf("got error %v, want %v", err, ErrQuery)
	}
	for _, want := range []string{"bad_data", "parse error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestQueryRangeNonPrometheusBody(t *testing.T) {
	client, _ := serve(t, http.StatusGatewayTimeout, "gateway_timeout.html")

	_, err := client.QueryRange(context.Background(), "up", at(0), at(2), step)
	if !errors.Is(err, ErrResponse) {
		t.Fatalf("got error %v, want %v", err, ErrResponse)
	}
	if !strings.Contains(err.Error(), "Gateway Time-out") {
		t.Errorf("error %q does not quote the body", err)
	}
}

func TestQueryRangeHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// released after the call returns, so the handler is still in flight when
	// the context is cancelled. This is the shape of someone pressing ctrl-c
	// on a backtest against a slow month.
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		cancel()
		<-released
	}))
	defer srv.Close()

	client, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client.HTTP = srv.Client()

	_, err = client.QueryRange(ctx, "up", at(0), at(2), step)
	close(released)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got error %v, want %v", err, context.Canceled)
	}
}

func TestQueryRangeCancellationStopsBetweenChunks(t *testing.T) {
	client, rec := serve(t, http.StatusOK, "chunk_first.json")
	client.MaxPointsPerSeries = 1

	ctx, cancel := context.WithCancel(context.Background())
	client.HTTP.Transport = cancelAfterFirst{rt: client.HTTP.Transport, cancel: cancel}

	_, err := client.QueryRange(ctx, "up", at(0), at(3), step)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got error %v, want %v", err, context.Canceled)
	}
	if len(rec.requests) != 1 {
		t.Errorf("got %d requests, want 1: a cancelled read must not start the next chunk",
			len(rec.requests))
	}
}

// cancelAfterFirst cancels the context once one request has completed, so the
// loop's check between chunks is what is being tested rather than the
// transport's.
type cancelAfterFirst struct {
	rt     http.RoundTripper
	cancel context.CancelFunc
}

func (c cancelAfterFirst) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(req)
	c.cancel()
	return resp, err
}

func TestQueryRangeSendsConfiguredHeaders(t *testing.T) {
	client, rec := serve(t, http.StatusOK, "matrix_empty.json")
	client.Header = http.Header{"X-Scope-Orgid": {"team-checkout"}}

	if _, err := client.QueryRange(context.Background(), "up", at(0), at(2), step); err != nil {
		t.Fatalf("QueryRange: %v", err)
	}

	if got := rec.headers[0].Get("X-Scope-OrgID"); got != "team-checkout" {
		t.Errorf("X-Scope-OrgID: got %q, want %q", got, "team-checkout")
	}
	if got := rec.headers[0].Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type: got %q", got)
	}
}

func TestQueryRangeRejectsUnusableRange(t *testing.T) {
	client, rec := serve(t, http.StatusOK, "matrix.json")

	if _, err := client.QueryRange(context.Background(), "up", at(10), at(0), step); !errors.Is(err, ErrRange) {
		t.Errorf("got error %v, want %v", err, ErrRange)
	}
	if _, err := client.QueryRange(context.Background(), "up", at(0), at(10), 0); !errors.Is(err, ErrStep) {
		t.Errorf("got error %v, want %v", err, ErrStep)
	}
	if len(rec.requests) != 0 {
		t.Errorf("got %d requests, want none: an unusable range is rejected before asking",
			len(rec.requests))
	}
}

func TestNewKeepsBasePathPrefix(t *testing.T) {
	client, rec := serve(t, http.StatusOK, "matrix_empty.json")
	// Mimir and ClickHouse both serve the API under a prefix, and dropping it
	// turns a working target into a 404 that reads like a wrong host.
	prefixed, err := New(client.base.Scheme + "://" + client.base.Host + "/prometheus")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	prefixed.HTTP = client.HTTP

	if _, err := prefixed.QueryRange(context.Background(), "up", at(0), at(2), step); err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if got, want := rec.paths[0], "/prometheus/api/v1/query_range"; got != want {
		t.Errorf("path: got %q, want %q", got, want)
	}
}

func TestNewRejectsUnusableBaseURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "empty", url: ""},
		{name: "no scheme", url: "localhost:9090"},
		{name: "not http", url: "unix:///var/run/prom.sock"},
		{name: "no host", url: "http://"},
		{name: "unparseable", url: "http://%zz"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.url); err == nil {
				t.Fatalf("New(%q): want an error", tc.url)
			}
		})
	}
}

// Points feed episode.Sample without a conversion layer: the timestamp is
// already a time.Time on the step grid, and what turns a value into a breach is
// the band, not an adapter in between.
func TestPointTimestampsAreUsableAsSampleTimes(t *testing.T) {
	client, _ := serve(t, http.StatusOK, "chunk_first.json")

	result, err := client.QueryRange(context.Background(), "up", at(0), at(1), step)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}

	points := result.Series[0].Points
	for i, p := range points {
		if math.IsNaN(p.Value) {
			continue
		}
		if off := p.At.Sub(at(0)) % step; off != 0 {
			t.Errorf("point %d at %s is %v off the step grid", i, p.At, off)
		}
	}
	if len(points) > 1 && !points[1].At.After(points[0].At) {
		t.Errorf("points are not in increasing time order: %s then %s", points[0].At, points[1].At)
	}
}

func TestQuery(t *testing.T) {
	client, rec := serve(t, http.StatusOK, "vector.json")
	client.Header = http.Header{"X-Scope-Orgid": {"team-checkout"}}

	result, err := client.Query(context.Background(), "up", at(0))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if result.Stats.Requests != 1 {
		t.Errorf("got %d requests, want 1", result.Stats.Requests)
	}
	if len(result.Series) != 2 {
		t.Fatalf("got %d series, want 2", len(result.Series))
	}
	for _, s := range result.Series {
		if len(s.Points) != 1 {
			t.Errorf("series %s has %d points, want 1", s.Key, len(s.Points))
		}
	}
	if got := result.Series[0].Points[0].Value; got != 1 {
		t.Errorf("first sample = %v, want 1", got)
	}
	if got := result.Series[1].Points[0].Value; !math.IsNaN(got) {
		t.Errorf("second sample = %v, want NaN", got)
	}

	if got := rec.paths[0]; got != "/api/v1/query" {
		t.Errorf("path = %q, want %q", got, "/api/v1/query")
	}
	form := rec.requests[0]
	if got := form.Get("query"); got != "up" {
		t.Errorf("query = %q, want %q", got, "up")
	}
	if form.Get("time") == "" {
		t.Error("no time in the request: an instant query says which instant")
	}
	for _, name := range []string{"start", "end", "step"} {
		if form.Get(name) != "" {
			t.Errorf("%s = %q, want nothing: an instant query has no range", name, form.Get(name))
		}
	}
	if got := rec.headers[0].Get("X-Scope-OrgID"); got != "team-checkout" {
		t.Errorf("X-Scope-OrgID: got %q, want %q", got, "team-checkout")
	}
}

func TestQueryEmptyResult(t *testing.T) {
	client, _ := serve(t, http.StatusOK, "vector_empty.json")

	result, err := client.Query(context.Background(), `up{job="nothing"}`, at(0))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Series) != 0 {
		t.Errorf("got %d series, want none", len(result.Series))
	}
}

func TestQueryRejectsRangeAnswer(t *testing.T) {
	client, _ := serve(t, http.StatusOK, "matrix.json")

	_, err := client.Query(context.Background(), "up", at(0))
	if !errors.Is(err, ErrResponse) {
		t.Fatalf("got error %v, want %v", err, ErrResponse)
	}
	if !strings.Contains(err.Error(), "vector") {
		t.Errorf("error %q does not say what it wanted", err)
	}
}

func TestQueryServerError(t *testing.T) {
	client, _ := serve(t, http.StatusBadRequest, "error_unknown_function.json")

	_, err := client.Query(context.Background(), "mad_over_time(up[1m])", at(0))
	if !errors.Is(err, ErrQuery) {
		t.Fatalf("got error %v, want %v", err, ErrQuery)
	}
	if !strings.Contains(err.Error(), "mad_over_time") {
		t.Errorf("error %q does not quote what the server said", err)
	}
}

// TestUnreachable separates the server that was never spoken to from the
// server that answered and said no. The gate a backtest runs in CI skips on
// the first and fails on the second, so classifying one as the other is a
// budget that stops being enforced or a broken query that stops being
// reported.
func TestUnreachable(t *testing.T) {
	// A server that existed and does not now, which is what a runner with no
	// network path to production meets: a real refused connection rather than
	// a fixture of one.
	srv := httptest.NewServer(http.NotFoundHandler())
	client, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.Close()

	_, err = client.QueryRange(context.Background(), "up", at(0), at(10), step)
	if err == nil {
		t.Fatal("QueryRange against a closed server returned no error")
	}
	if !Unreachable(err) {
		t.Errorf("Unreachable(%v) = false, want true", err)
	}

	rejected, _ := serve(t, http.StatusBadRequest, "error_unknown_function.json")
	_, err = rejected.QueryRange(context.Background(), "mad_over_time(up[1m])", at(0), at(10), step)
	if !errors.Is(err, ErrQuery) {
		t.Fatalf("got error %v, want %v", err, ErrQuery)
	}
	if Unreachable(err) {
		t.Errorf("Unreachable(%v) = true, want false: the server answered", err)
	}

	if Unreachable(nil) {
		t.Error("Unreachable(nil) = true")
	}
}
