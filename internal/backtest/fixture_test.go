package backtest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dennisme/feno/internal/prometheus"
	"github.com/dennisme/feno/internal/spec"
)

// The window every fixture is read and reported over.
//
// One hour, not one minute. What these tests check is the grid, the warm-up
// boundary, the aggregation across series and the report, none of which know
// what a sample is worth, and a fixture carrying a month of minutes would be
// forty thousand numbers nobody reviews. The detector fixtures carry windows
// and hold durations scaled to match, so a three day seasonal lookback is
// still longer than the reported window, which is the case that matters.
var (
	step       = time.Hour
	reportFrom = time.Date(2026, 9, 4, 6, 0, 0, 0, time.UTC)
	reportTo   = time.Date(2026, 9, 5, 6, 0, 0, 0, time.UTC)
)

// fixture is a history file: one start, one step, and a run-length description
// of each series.
//
// Run lengths rather than a list of samples, because the interesting fixtures
// are four days long and a list of them is unreviewable. A segment is a value
// repeated, optionally alternating a jitter either side of it so that a window
// has a scale at all: with an even number of samples per window the alternation
// makes the mean exactly value and the standard deviation exactly jitter, which
// is what lets every expectation below be worked out by hand.
type fixture struct {
	Comment string          `json:"comment"`
	Start   string          `json:"start"`
	Step    string          `json:"step"`
	Series  []fixtureSeries `json:"series"`
}

type fixtureSeries struct {
	Labels   map[string]string `json:"labels"`
	Segments []segment         `json:"segments"`
}

type segment struct {
	Repeat int     `json:"repeat"`
	Value  float64 `json:"value"`
	Jitter float64 `json:"jitter"`

	// Absent leaves the instants with no sample, which is a series that was
	// not being scraped rather than one that was flat.
	Absent bool `json:"absent"`
}

// load reads a detector fixture.
func load(t *testing.T, name string) spec.Detector {
	t.Helper()
	bundle, err := spec.Load(filepath.Join("testdata", "detectors", name))
	if err != nil {
		t.Fatalf("loading %s: %v", name, err)
	}
	if len(bundle.Detectors) != 1 {
		t.Fatalf("%s holds %d detectors, want 1", name, len(bundle.Detectors))
	}
	return bundle.Detectors[0]
}

// history reads a history fixture and returns it as the reader would have.
//
// The fixture's start has to be the window's read start, so a fixture that
// drifted away from the window the tests use fails here rather than producing
// a report of absences that looks like a finding.
func history(t *testing.T, name string, w Window) *prometheus.Result {
	t.Helper()

	// G304 is reading a path built from a variable, which is what a fixture
	// loader does. Excluded in _test.go by the lint configuration, and named
	// here because the loader in internal/spec carries the same note.
	data, err := os.ReadFile(filepath.Join("testdata", "history", name)) //nolint:gosec
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}

	var f fixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&f); err != nil {
		t.Fatalf("decoding %s: %v", name, err)
	}

	start, err := time.Parse(time.RFC3339, f.Start)
	if err != nil {
		t.Fatalf("%s: start %q: %v", name, f.Start, err)
	}
	fixtureStep, err := time.ParseDuration(f.Step)
	if err != nil {
		t.Fatalf("%s: step %q: %v", name, f.Step, err)
	}
	if !start.Equal(w.ReadStart) {
		t.Fatalf("%s starts at %s, the window reads from %s", name, f.Start, instant(w.ReadStart))
	}
	if fixtureStep != w.Step {
		t.Fatalf("%s is at step %v, the window is at %v", name, fixtureStep, w.Step)
	}

	result := &prometheus.Result{Stats: prometheus.Stats{Requests: 1}}
	for _, s := range f.Series {
		series := prometheus.Series{
			Key:    prometheus.SeriesKey(s.Labels),
			Labels: s.Labels,
		}
		var index int
		for _, seg := range s.Segments {
			for range seg.Repeat {
				at := start.Add(time.Duration(index) * fixtureStep)
				index++
				if seg.Absent {
					continue
				}
				value := seg.Value
				// The sign alternates on the sample's position in the whole
				// series rather than within the segment, so the alternation
				// survives a segment boundary and every even-length window
				// still holds as many of one sign as the other.
				if index%2 == 1 {
					value += seg.Jitter
				} else {
					value -= seg.Jitter
				}
				series.Points = append(series.Points, prometheus.Point{At: at, Value: value})
			}
		}
		result.Stats.Points += len(series.Points)
		result.Series = append(result.Series, series)
	}
	return result
}

// window is the window a fixture is read over, for the given detector.
func window(t *testing.T, d spec.Detector) Window {
	t.Helper()
	band, err := d.Band(step)
	if err != nil {
		t.Fatalf("band: %v", err)
	}
	w, err := NewWindow(reportFrom, reportTo, step, band.Lookback())
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	return w
}

// analyse is the whole slice over one detector and one history fixture.
func analyse(t *testing.T, detector, file string) *Report {
	t.Helper()
	d := load(t, detector)
	w := window(t, d)
	report, err := Analyse(d, w, history(t, file, w))
	if err != nil {
		t.Fatalf("analysing %s over %s: %v", detector, file, err)
	}
	return report
}
