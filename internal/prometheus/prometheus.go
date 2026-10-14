// Package prometheus reads a PromQL query from any Prometheus-compatible HTTP
// API and returns the series it found, and asks one server which optional
// functions it has.
//
// This is the backtest's input. A detector is judged on real history, so the
// first thing the tool has to do well is fetch a month of it from whatever is
// already running: Prometheus, Mimir, Thanos, VictoriaMetrics, AMP, or
// ClickHouse's Prometheus-compatible endpoint. All of them answer
// `/api/v1/query_range`, which is what the backtest reads, and
// `/api/v1/query`, which is what [Client.Probe] asks a single question of.
// Reading is all this package does.
//
// # Why there is no Reader interface
//
// ClickHouse will eventually be read through its own SQL rather than this
// endpoint, and a cross-target interface written now would be a guess at what
// that reader needs. One concrete reader, and the interface when there is a
// second implementation to derive it from.
//
// # Why requests are not concurrent
//
// A range longer than the server's resolution limit becomes several requests
// (see planChunks), and they go out one at a time. The target is somebody's
// production monitoring, often the one thing still answering during the
// incident this detector is meant to catch, and a backtest that fans out
// twenty range queries at it is a tool nobody is allowed to run twice.
//
// # Series identity
//
// Every series carries a Key from [SeriesKey], and the rest of the tool depends
// on that key being the identity of a label set: the report counts distinct
// instances by it, and an episode is traced back to its series by it.
//
// # What is not tested
//
// Most tests here use an httptest.Server serving recorded fixtures, so what
// they cover is this package: the chunk planner, the stitching, the parser and
// the error paths. They prove nothing about how a real Prometheus behaves, and
// no fixture should be read as evidence of server semantics.
//
// The probe is the exception, because it reads another project's wording to
// decide what feno compiles, and a fixture of that wording is evidence of
// nothing but itself. integration_test.go, behind a build tag, runs it against
// a server started both ways; `just smoketest` is where the two servers come
// from. What a real server does to a month-long chunked range query is still
// unproven here, and internal/backtest's integration test is where that claim
// belongs.
package prometheus

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// maxBodyBytes caps what one response may spend memory on. A legitimate answer
// at the resolution limit is a few megabytes; anything near this cap is a
// misrouted request to something that streams, and reading it to the end would
// be the backtest's only visible symptom.
const maxBodyBytes = 64 << 20

// The API paths are appended to the configured base URL, so a base carrying a
// prefix like Mimir's `/prometheus` or ClickHouse's `/prometheus` keeps it.
const (
	queryRangePath = "api/v1/query_range"
	queryPath      = "api/v1/query"
)

// Point is one sample: when it was evaluated and what it was worth. Value can
// be NaN or an infinity, because the server reports those and a backtest that
// silently dropped them would read a broken counter as a quiet hour.
type Point struct {
	At    time.Time
	Value float64
}

// Series is one label set and the samples that belong to it, in increasing
// time order, stitched across however many requests the range needed.
type Series struct {
	Key    string
	Labels map[string]string
	Points []Point
}

// Stats is what the read cost and what the server said about it.
//
// Warnings are here rather than logged because they change what the numbers
// mean: a query trimmed by a server-side limit returns fewer points, and
// without the warning a trimmed month reads as a quiet one.
type Stats struct {
	Requests int
	Points   int
	Warnings []string
}

// Result is the answer to one range query.
type Result struct {
	Series []Series
	Stats  Stats
}

// Client reads queries from one server.
//
// Header is sent with every request, which is how a multi-tenant target is
// reached at all: Mimir and Thanos require X-Scope-OrgID, and an
// authenticating proxy in front of any of them requires whatever it requires.
//
// MaxPointsPerSeries overrides the resolution limit requests are planned
// against. Zero means the Prometheus default, and the reason to set it is a
// server configured lower than that.
type Client struct {
	HTTP               *http.Client
	Header             http.Header
	MaxPointsPerSeries int

	base *url.URL
}

// New builds a client for a base URL such as http://localhost:9090 or
// http://mimir.internal/prometheus.
func New(baseURL string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("base url %q: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("base url %q: scheme must be http or https", baseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("base url %q: no host", baseURL)
	}
	return &Client{base: u}, nil
}

// QueryRange evaluates query at every step between start and end.
//
// The range is split into as many requests as the resolution limit demands and
// the results are stitched back into one series per label set. It returns on
// the first failure: a partial month of history would produce an episode report
// that reads as a complete one.
func (c *Client) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (*Result, error) {
	chunks, err := planChunks(start, end, step, c.MaxPointsPerSeries)
	if err != nil {
		return nil, err
	}

	var result Result
	index := make(map[string]int)

	for _, ch := range chunks {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%s..%s: %w", ch.start, ch.end, err)
		}

		series, warnings, err := c.queryRange(ctx, query, ch, step)
		if err != nil {
			return nil, fmt.Errorf("%s..%s step %v: %w", ch.start, ch.end, step, err)
		}

		result.Stats.Requests++
		for _, w := range warnings {
			if !slices.Contains(result.Stats.Warnings, w) {
				result.Stats.Warnings = append(result.Stats.Warnings, w)
			}
		}

		for _, s := range series {
			result.Stats.Points += len(s.Points)
			if i, seen := index[s.Key]; seen {
				result.Series[i].Points = append(result.Series[i].Points, s.Points...)
				continue
			}
			index[s.Key] = len(result.Series)
			result.Series = append(result.Series, s)
		}
	}
	return &result, nil
}

// queryRange performs one range request.
func (c *Client) queryRange(ctx context.Context, query string, ch chunk, step time.Duration) ([]Series, []string, error) {
	form := url.Values{
		"query": {query},
		"start": {formatTime(ch.start)},
		"end":   {formatTime(ch.end)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}

	status, body, err := c.post(ctx, c.base.JoinPath(queryRangePath), form)
	if err != nil {
		return nil, nil, err
	}
	return parseResponse(status, body)
}

// Query evaluates query at one instant.
//
// This is how an optional server feature is detected: the answer to a query
// that uses it says whether the server has it, and nothing in a specification
// file can. See [Probe].
func (c *Client) Query(ctx context.Context, query string, at time.Time) (*Result, error) {
	form := url.Values{
		"query": {query},
		"time":  {formatTime(at)},
	}

	status, body, err := c.post(ctx, c.base.JoinPath(queryPath), form)
	if err != nil {
		return nil, err
	}
	series, warnings, err := parseInstant(status, body)
	if err != nil {
		return nil, err
	}

	result := &Result{Series: series, Stats: Stats{Requests: 1, Warnings: warnings}}
	for _, s := range series {
		result.Stats.Points += len(s.Points)
	}
	return result, nil
}

// post performs one request.
//
// It is a POST because a generated band expression runs to hundreds of
// characters and a GET of it meets a proxy's URL length limit as a 414 that
// looks like the query is wrong.
func (c *Client) post(ctx context.Context, endpoint *url.URL, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("building request: %w", err)
	}
	for name, values := range c.Header {
		req.Header[http.CanonicalHeaderKey(name)] = slices.Clone(values)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("requesting %s: %w", endpoint, err)
	}
	// A close failure on a body already read to the end says nothing the
	// caller can act on, and reporting it would mask the parse error above.
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("reading response: %w", err)
	}
	return resp.StatusCode, body, nil
}

// formatTime renders a timestamp the way the API documents it, as a unix
// timestamp with an optional fraction. RFC 3339 is also accepted, but every
// server agrees on the number.
func formatTime(t time.Time) string {
	seconds := float64(t.Unix()) + float64(t.Nanosecond())/1e9
	return strconv.FormatFloat(seconds, 'f', -1, 64)
}
