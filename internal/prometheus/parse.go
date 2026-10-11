package prometheus

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrQuery reports a query the server rejected, which is a specification
	// or a PromQL problem rather than a transport one.
	ErrQuery = errors.New("query rejected by server")

	// ErrResponse reports a body that is not a usable range query answer,
	// whether it came from a proxy in the way or a server speaking a different
	// shape of the API.
	ErrResponse = errors.New("unusable response")
)

// snippetLen is how much of an unparseable body goes into the error. Enough to
// recognise an nginx error page or an HTML login form, short enough to read in
// a terminal.
const snippetLen = 256

// wireResponse is the envelope every endpoint of the HTTP API answers with.
// Both the success and the error shapes are here because which one arrived is
// a field rather than a status code: Mimir and Thanos sit behind proxies that
// rewrite statuses, so the body is what gets believed.
type wireResponse struct {
	Status    string   `json:"status"`
	Data      wireData `json:"data"`
	ErrorType string   `json:"errorType"`
	Error     string   `json:"error"`
	Warnings  []string `json:"warnings"`
}

type wireData struct {
	ResultType string       `json:"resultType"`
	Result     []wireSeries `json:"result"`
}

// wireSeries holds samples as raw JSON because the pair is heterogeneous: a
// number for the timestamp and a string for the sample. Decoding it as any
// would route every value through float64 and lose NaN and the infinities,
// which only survive the wire because the sample is a string.
type wireSeries struct {
	Metric map[string]string `json:"metric"`
	Values []json.RawMessage `json:"values"`
}

// parseResponse turns one range query answer into series and the warnings that
// came with it.
func parseResponse(statusCode int, body []byte) ([]Series, []string, error) {
	var resp wireResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, fmt.Errorf("http %d: body is not JSON: %q: %w",
			statusCode, snippet(body), ErrResponse)
	}

	if resp.Status == "error" {
		return nil, nil, fmt.Errorf("http %d: %s: %s: %w",
			statusCode, orUnknown(resp.ErrorType), orUnknown(resp.Error), ErrQuery)
	}
	if statusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("http %d with status %q: %q: %w",
			statusCode, resp.Status, snippet(body), ErrResponse)
	}
	if resp.Data.ResultType != "matrix" {
		return nil, nil, fmt.Errorf("result type %q, want matrix: %w",
			resp.Data.ResultType, ErrResponse)
	}

	series := make([]Series, 0, len(resp.Data.Result))
	for _, w := range resp.Data.Result {
		points, err := parsePoints(w.Values)
		if err != nil {
			return nil, nil, fmt.Errorf("series %s: %w", SeriesKey(w.Metric), err)
		}
		series = append(series, Series{
			Key:    SeriesKey(w.Metric),
			Labels: w.Metric,
			Points: points,
		})
	}
	return series, resp.Warnings, nil
}

func parsePoints(values []json.RawMessage) ([]Point, error) {
	points := make([]Point, 0, len(values))
	for i, raw := range values {
		var pair []json.RawMessage
		if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
			return nil, fmt.Errorf("sample %d is not a timestamp and a value: %s: %w",
				i, raw, ErrResponse)
		}

		var ts float64
		if err := json.Unmarshal(pair[0], &ts); err != nil {
			return nil, fmt.Errorf("sample %d timestamp %s: %w", i, pair[0], ErrResponse)
		}
		var text string
		if err := json.Unmarshal(pair[1], &text); err != nil {
			return nil, fmt.Errorf("sample %d value %s is not a string: %w", i, pair[1], ErrResponse)
		}
		// ParseFloat is what accepts NaN, +Inf and -Inf, which a server emits
		// for a counter that reset into a division by zero or a rate over a
		// saturated gauge.
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("sample %d value %q: %w", i, text, ErrResponse)
		}

		points = append(points, Point{At: timestamp(ts), Value: value})
	}
	return points, nil
}

// timestamp converts the wire's float seconds to a time.
//
// Prometheus holds sample times as milliseconds and prints them as a JSON
// float, so the fraction is exact at that resolution and rounding recovers it.
// Truncating instead would move a point a millisecond earlier, which is enough
// to put it off the step grid the episode extractor reads.
func timestamp(seconds float64) time.Time {
	return time.UnixMilli(int64(math.Round(seconds * 1000))).UTC()
}

// SeriesKey is the stable identity of a label set.
//
// Everything downstream joins on this: the report counts distinct instances by
// it, and an episode is traced back to the series behind it by it. So it has to
// be independent of map iteration order, which is why the pairs are sorted, and
// it has to be injective, which is why both halves of every pair are quoted. A
// plain join on `=` and `,` would read a label value containing either as a
// second label and report two series as one.
func SeriesKey(labels map[string]string) string {
	pairs := make([]string, 0, len(labels))
	for name, value := range labels {
		pairs = append(pairs, strconv.Quote(name)+"="+strconv.Quote(value))
	}
	slices.Sort(pairs)
	return strings.Join(pairs, ",")
}

// snippet renders the head of a body as one line, so an HTML error page does
// not spill fifteen lines into a test failure or a terminal.
func snippet(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > snippetLen {
		text = text[:snippetLen] + "..."
	}
	return text
}

// orUnknown names a field the server left empty, because "http 400: : : query
// rejected" tells a reader nothing about which part was missing.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
