package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEmit(t *testing.T) {
	end := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := emit(&out, 2*24*time.Hour, time.Minute, end); err != nil {
		t.Fatalf("emit: %v", err)
	}
	text := out.String()

	if strings.Count(text, "# TYPE ") != len(metrics) || strings.Count(text, "# HELP ") != len(metrics) {
		t.Errorf("want one HELP and one TYPE line per metric, got\n%s", firstLines(text, 8))
	}
	if !strings.HasSuffix(text, "# EOF\n") {
		t.Error("OpenMetrics input has to end with # EOF, which is what promtool parses to")
	}

	// Per series, so a missing sample in one of them is not hidden by the
	// other being complete.
	samples := map[string][]sample{}
	for line := range strings.Lines(text) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, series, err := parseSample(line)
		if err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		samples[series] = append(samples[series], s)
	}

	// Keyed by metric and label set together, because every metric here
	// carries the same label sets and a key of labels alone would hide a
	// family that was never written.
	if want := len(metrics) * len(services); len(samples) != want {
		t.Fatalf("got %d series, want one per metric per service (%d)", len(samples), want)
	}
	for labels, got := range samples {
		t.Run(labels, func(t *testing.T) {
			// Both ends included: the first instant of the range and the last
			// are both evaluated, which is what a range query returns.
			want := int((2*24*time.Hour)/time.Minute) + 1
			if len(got) != want {
				t.Errorf("got %d samples, want %d", len(got), want)
			}
			if last := got[len(got)-1].at; !last.Equal(end) {
				t.Errorf("last sample at %s, want the end of the range %s", last, end)
			}
			for i := 1; i < len(got); i++ {
				if gap := got[i].at.Sub(got[i-1].at); gap != time.Minute {
					t.Fatalf("sample %d is %s after the one before it, want one step", i, gap)
				}
			}

			// A flat series gives an adaptive band with daily seasonality
			// nothing to fit, so the seed would prove the read and not the
			// detection. One day's spread is what says it has shape.
			day := got[len(got)-1440:]
			low, high := day[0].value, day[0].value
			for _, s := range day {
				low, high = min(low, s.value), max(high, s.value)
			}
			if high-low < 1 {
				t.Errorf("the last day spans %v..%v, which is flat", low, high)
			}
		})
	}
}

type sample struct {
	at    time.Time
	value float64
}

// parseSample reads one OpenMetrics sample line into its series name and its
// point, and is strict about the fields promtool requires: a timestamp is
// not optional here, because an import without one is rejected.
func parseSample(line string) (sample, string, error) {
	open, close := strings.Index(line, "{"), strings.LastIndex(line, "}")
	if open < 0 || close < 0 {
		return sample{}, "", fmt.Errorf("no label set")
	}
	series := line[:close+1]

	fields := strings.Fields(line[close+1:])
	if len(fields) != 2 {
		return sample{}, "", fmt.Errorf("got %d fields after the label set, want a value and a timestamp", len(fields))
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return sample{}, "", err
	}
	seconds, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return sample{}, "", err
	}
	return sample{at: time.Unix(seconds, 0).UTC(), value: value}, series, nil
}

func firstLines(text string, n int) string {
	lines := strings.SplitN(text, "\n", n+1)
	return strings.Join(lines[:min(n, len(lines))], "\n")
}

// TestBurnSignalCrossesTheAllowance is what makes the burn half of the seed
// worth writing: the error ratio has to exceed what a 99.9% objective allows
// somewhere in the span, or the backtest's budget overlap would be read
// against a budget that never burned and the two grids could be anywhere.
func TestBurnSignalCrossesTheAllowance(t *testing.T) {
	end := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := emit(&out, 7*24*time.Hour, time.Minute, end); err != nil {
		t.Fatalf("emit: %v", err)
	}

	// Total and good per instant per service, so the ratio is taken between
	// the two samples that share a timestamp.
	type pair struct{ total, good float64 }
	points := map[string]pair{}
	for line := range strings.Lines(out.String()) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, series, err := parseSample(line)
		if err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		name, labels, _ := strings.Cut(series, "{")
		key := labels + strconv.FormatInt(s.at.Unix(), 10)
		got := points[key]
		switch name {
		case requestMetric:
			got.total = s.value
		case goodMetric:
			got.good = s.value
		default:
			t.Fatalf("unknown metric %q", name)
		}
		points[key] = got
	}

	// A 99.9% objective leaves a thousandth, so the burn rate is the error
	// ratio over that: above one the budget is being spent too fast.
	const allowance = 0.001

	var burning, quiet int
	for key, got := range points {
		if got.total <= 0 {
			t.Fatalf("%s: total of %v, which no ratio divides by", key, got.total)
		}
		if got.good > got.total {
			t.Fatalf("%s: good %v above total %v", key, got.good, got.total)
		}
		if (got.total-got.good)/got.total > allowance {
			burning++
			continue
		}
		quiet++
	}

	if burning == 0 {
		t.Error("the error ratio never crossed what the objective allows, so nothing ever burned")
	}
	if quiet == 0 {
		t.Error("the error ratio never fell inside what the objective allows, so the budget burned all month")
	}
}
