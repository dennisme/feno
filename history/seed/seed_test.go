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

	if strings.Count(text, "# TYPE ") != 1 || strings.Count(text, "# HELP ") != 1 {
		t.Errorf("want one HELP and one TYPE line, got\n%s", firstLines(text, 4))
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
		s, labels, err := parseSample(line)
		if err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		samples[labels] = append(samples[labels], s)
	}

	if len(samples) != len(services) {
		t.Fatalf("got %d series, want one per service (%d)", len(samples), len(services))
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

// parseSample reads one OpenMetrics sample line into its labels and its
// point, and is strict about the fields promtool requires: a timestamp is
// not optional here, because an import without one is rejected.
func parseSample(line string) (sample, string, error) {
	open, close := strings.Index(line, "{"), strings.LastIndex(line, "}")
	if open < 0 || close < 0 {
		return sample{}, "", fmt.Errorf("no label set")
	}
	labels := line[open : close+1]

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
	return sample{at: time.Unix(seconds, 0).UTC(), value: value}, labels, nil
}

func firstLines(text string, n int) string {
	lines := strings.SplitN(text, "\n", n+1)
	return strings.Join(lines[:min(n, len(lines))], "\n")
}
