package backtest

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/feno/internal/prometheus"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v (run with -update)", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s does not match:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// TestNewWindow covers the boundary the whole slice turns on: what is read
// against what is reported on.
func TestNewWindow(t *testing.T) {
	from := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		to        time.Time
		step      time.Duration
		lookback  time.Duration
		readStart time.Time
		end       time.Time
		instants  int
		err       error
	}{
		{
			// A weekly band over thirty days: three weeks of offsets plus the
			// window, so fifty-one days are read and thirty are reported on.
			name:      "a seasonal lookback is read before the reported window",
			to:        from.Add(30 * 24 * time.Hour),
			step:      time.Minute,
			lookback:  21*24*time.Hour + time.Hour,
			readStart: from.Add(-(21*24*time.Hour + time.Hour)),
			end:       from.Add(30 * 24 * time.Hour),
			instants:  30*24*60 + 1,
		},
		{
			// The warm-up is rounded up to a whole step, because
			// internal/detect matches a sample to an instant by exact
			// timestamp and a read starting half a step early contributes
			// nothing at all.
			name:      "a lookback that is not a whole number of steps rounds up",
			to:        from.Add(6 * time.Hour),
			step:      time.Hour,
			lookback:  90 * time.Minute,
			readStart: from.Add(-2 * time.Hour),
			end:       from.Add(6 * time.Hour),
			instants:  7,
		},
		{
			name:      "no lookback still reports the range asked for",
			to:        from.Add(time.Hour),
			step:      time.Minute,
			lookback:  0,
			readStart: from,
			end:       from.Add(time.Hour),
			instants:  61,
		},
		{
			// A range query evaluates at its start and then every step, so a
			// final remainder shorter than a step holds no instant.
			name:      "a range that is not a multiple of the step is truncated",
			to:        from.Add(90 * time.Minute),
			step:      time.Hour,
			lookback:  time.Hour,
			readStart: from.Add(-time.Hour),
			end:       from.Add(time.Hour),
			instants:  2,
		},
		{
			name:      "a single instant is a window",
			to:        from,
			step:      time.Hour,
			lookback:  time.Hour,
			readStart: from.Add(-time.Hour),
			end:       from,
			instants:  1,
		},
		{
			name: "a step of zero is refused",
			to:   from.Add(time.Hour),
			step: 0,
			err:  ErrStep,
		},
		{
			name: "an end before the start is refused",
			to:   from.Add(-time.Hour),
			step: time.Hour,
			err:  ErrRange,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := NewWindow(from, tt.to, tt.step, tt.lookback)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("error = %v, want %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewWindow: %v", err)
			}
			if !w.Start.Equal(from) {
				t.Errorf("Start = %s, want %s", instant(w.Start), instant(from))
			}
			if !w.End.Equal(tt.end) {
				t.Errorf("End = %s, want %s", instant(w.End), instant(tt.end))
			}
			if !w.ReadStart.Equal(tt.readStart) {
				t.Errorf("ReadStart = %s, want %s", instant(w.ReadStart), instant(tt.readStart))
			}
			if got := w.WarmUp(); got != w.Start.Sub(tt.readStart) {
				t.Errorf("WarmUp = %v, want %v", got, w.Start.Sub(tt.readStart))
			}
			if got := w.Instants(); got != tt.instants {
				t.Errorf("Instants = %d, want %d", got, tt.instants)
			}
			if got := w.Read(); got != w.Reported()+w.WarmUp() {
				t.Errorf("Read = %v, want %v", got, w.Reported()+w.WarmUp())
			}
		})
	}
}

// TestAnalyse is the aggregation, over fixtures whose expectations are worked
// out by hand from the band the detector compiles to.
//
// Every case reads the same twenty-five hour grid at a one hour step. The
// seasonal detector pools three six hour windows at a day, two days and three
// days back, which on a series alternating one either side of a hundred is a
// centre of 100 and a scale of 1, so a balanced band is [97, 103] and a sample
// of 140 breaches it.
func TestAnalyse(t *testing.T) {
	tests := []struct {
		name     string
		detector string
		history  string

		series   int
		counts   Counts
		episodes int
		flaps    int
		firing   time.Duration
		paged    time.Duration
		peak     int
		peakAt   time.Time
	}{
		{
			// The lookback is three days and six hours against a reported
			// window of one day, so the warm-up is longer than the report.
			// Every reported instant is evaluable because the read covered it.
			name:     "a warm-up longer than the reported window leaves nothing cold",
			detector: "daily-seasonal.yaml",
			history:  "warmup.json",
			series:   1,
			counts:   Counts{Evaluated: 25},
			episodes: 1,
			firing:   2 * time.Hour,
			paged:    2 * time.Hour,
			peak:     1,
			peakAt:   time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC),
		},
		{
			// The series begins twenty hours into the read, so the pooled
			// window of the earliest reported instants holds twelve or
			// thirteen samples against the fourteen the coverage policy
			// demands. Fifteen instants are cold and the last ten are not.
			// The sample is present at every one of them, so none of this is
			// absence: a backtest reporting these as quiet would be reporting
			// an empty warm-up as a clean detector.
			name:     "history that starts late is cold rather than quiet",
			detector: "daily-seasonal.yaml",
			history:  "late-start.json",
			series:   1,
			counts:   Counts{Evaluated: 10, Cold: 15},
		},
		{
			// Two instances spike over overlapping hours and a third never
			// does. Two episodes, because an alerting expression produces one
			// alert per label set, and a peak of two series firing at once.
			name:     "several series from one query are counted per series",
			detector: "daily-seasonal.yaml",
			history:  "multi-series.json",
			series:   3,
			counts:   Counts{Evaluated: 25},
			episodes: 2,
			firing:   5 * time.Hour,
			paged:    3 * time.Hour,
			peak:     2,
			peakAt:   time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC),
		},
		{
			// One missing evaluation in the middle of a four sample breach.
			// Prometheus drops a pending alert on a single missed evaluation,
			// so this is two runs of two samples rather than one run of four,
			// and at a two hour hold neither survives.
			name:     "a missing evaluation ends the run it falls in",
			detector: "daily-seasonal.yaml",
			history:  "absent.json",
			series:   1,
			counts:   Counts{Evaluated: 24, Absent: 1},
			flaps:    2,
		},
		{
			// The same history as the multi-series case against a hold
			// duration longer than either breach. Everything crosses the band
			// and nothing reaches Alertmanager, which is the shape of a rule
			// whose `for` is carrying it.
			name:     "a hold duration longer than every breach pages nobody",
			detector: "patient.yaml",
			history:  "multi-series.json",
			series:   3,
			counts:   Counts{Evaluated: 25},
			flaps:    2,
		},
		{
			name:     "a query that matched nothing reports no series",
			detector: "daily-seasonal.yaml",
			history:  "no-series.json",
			series:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := analyse(t, tt.detector, tt.history)

			if got := len(report.Series); got != tt.series {
				t.Fatalf("series = %d, want %d", got, tt.series)
			}
			if tt.series > 0 {
				if got := report.Series[0].Counts; got != tt.counts {
					t.Errorf("counts of the worst series = %+v, want %+v", got, tt.counts)
				}
			}
			if got := report.Episodes(); got != tt.episodes {
				t.Errorf("paging episodes = %d, want %d", got, tt.episodes)
			}
			if got := report.Flaps(); got != tt.flaps {
				t.Errorf("flaps = %d, want %d", got, tt.flaps)
			}
			if got := report.SeriesFiringTime(); got != tt.firing {
				t.Errorf("series firing time = %v, want %v", got, tt.firing)
			}
			if got := report.PagedTime(); got != tt.paged {
				t.Errorf("paged wall clock = %v, want %v", got, tt.paged)
			}
			if got := report.Peak; got != tt.peak {
				t.Errorf("peak concurrent = %d, want %d", got, tt.peak)
			}
			if !report.PeakAt.Equal(tt.peakAt) {
				t.Errorf("peak at %s, want %s", instant(report.PeakAt), instant(tt.peakAt))
			}
		})
	}
}

// TestWorstSeriesFirst is the other half of "which series caused what": the
// order has to be the same every run, or a report cannot be diffed.
func TestWorstSeriesFirst(t *testing.T) {
	report := analyse(t, "daily-seasonal.yaml", "multi-series.json")

	want := []string{"web-2", "web-1", "web-3"}
	for i, instance := range want {
		if got := report.Series[i].Labels["instance"]; got != instance {
			t.Errorf("series %d is %q, want %q", i, got, instance)
		}
	}

	where, longest, ok := report.Longest()
	if !ok {
		t.Fatal("Longest reports nothing fired")
	}
	if got := where.Labels["instance"]; got != "web-2" {
		t.Errorf("longest episode belongs to %q, want web-2", got)
	}
	if got := longest.FiringDuration(); got != 3*time.Hour {
		t.Errorf("longest firing duration = %v, want 3h", got)
	}
}

// TestRatioDenominator pins the denominator the package comment argues for.
// Summed series firing time can exceed the window, and that is the honest
// reading rather than a bug: two instances firing all month is two months of
// pages.
func TestRatioDenominator(t *testing.T) {
	report := analyse(t, "daily-seasonal.yaml", "multi-series.json")

	if got := report.Window.Reported(); got != 24*time.Hour {
		t.Fatalf("reported window = %v, want 24h", got)
	}
	// Five series-hours and three wall-clock hours over a twenty-four hour
	// window.
	if got := report.Ratio(report.SeriesFiringTime()); got != 5.0/24.0 {
		t.Errorf("series firing ratio = %v, want %v", got, 5.0/24.0)
	}
	if got := report.Ratio(report.PagedTime()); got != 3.0/24.0 {
		t.Errorf("paged ratio = %v, want %v", got, 3.0/24.0)
	}

	// The alternative that flatters: the span the samples cover is the whole
	// read, warm-up included, which is four times the window here and would
	// report a quarter of the real ratio.
	if report.Window.Read() <= report.Window.Reported() {
		t.Fatal("the read span is not longer than the reported window, so the case is not being exercised")
	}
}

func TestThresholds(t *testing.T) {
	report := analyse(t, "daily-seasonal.yaml", "multi-series.json")

	tests := []struct {
		name       string
		thresholds Thresholds
		findings   int
		inFinding  string
	}{
		{
			name:       "nothing set is nothing exceeded",
			thresholds: NoThresholds,
		},
		{
			name:       "two episodes against a limit of two",
			thresholds: Thresholds{MaxEpisodes: 2, MaxFiringRatio: -1},
		},
		{
			name:       "two episodes against a limit of one",
			thresholds: Thresholds{MaxEpisodes: 1, MaxFiringRatio: -1},
			findings:   1,
		},
		{
			// Zero is a limit somebody means: a detector that may never page.
			name:       "a limit of zero episodes is a limit, not an absence",
			thresholds: Thresholds{MaxEpisodes: 0, MaxFiringRatio: -1},
			findings:   1,
		},
		{
			name:       "paged for an eighth of the window against a limit of a tenth",
			thresholds: Thresholds{MaxEpisodes: -1, MaxFiringRatio: 0.1},
			findings:   1,
		},
		{
			name:       "both limits broken",
			thresholds: Thresholds{MaxEpisodes: 0, MaxFiringRatio: 0},
			findings:   2,
		},
		{
			// Over budget is not actionable until the reader knows which
			// file set the budget, which in a repository with a budget per
			// team is not the one they are looking at.
			name: "the file that set the limit is named",
			thresholds: Thresholds{
				MaxEpisodes: 0, MaxFiringRatio: -1,
				EpisodesFrom: "teams/checkout/feno.yaml",
			},
			findings:  1,
			inFinding: "(teams/checkout/feno.yaml)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := report.Exceeded(tt.thresholds)
			if len(got) != tt.findings {
				t.Errorf("findings = %d (%v), want %d", len(got), got, tt.findings)
			}
			if tt.inFinding != "" && !strings.Contains(strings.Join(got, "\n"), tt.inFinding) {
				t.Errorf("findings = %v, want one to contain %q", got, tt.inFinding)
			}
		})
	}
}

// TestAnalyseErrors covers the two mistakes a caller can make that would
// otherwise produce a plausible report of the wrong thing.
func TestAnalyseErrors(t *testing.T) {
	t.Run("a warm-up shorter than the band's lookback", func(t *testing.T) {
		d := load(t, "daily-seasonal.yaml")
		// A window built for a band with no history requirement at all, used
		// with a band that needs three days of it.
		w, err := NewWindow(reportFrom, reportTo, step, 0)
		if err != nil {
			t.Fatalf("NewWindow: %v", err)
		}
		if _, err := Analyse(d, w, &prometheus.Result{}, nil); !errors.Is(err, ErrWarmUp) {
			t.Errorf("error = %v, want %v", err, ErrWarmUp)
		}
	})

	t.Run("a datasource this reader does not speak", func(t *testing.T) {
		d := load(t, "clickhouse.yaml")
		w := window(t, d)
		if _, err := Analyse(d, w, &prometheus.Result{}, nil); !errors.Is(err, ErrSourceType) {
			t.Errorf("error = %v, want %v", err, ErrSourceType)
		}
	})
}

// TestReport is the report's own formatting. The numbers are checked above;
// what this pins is that a reader can tell which window a ratio is over and
// which series caused what.
func TestReport(t *testing.T) {
	tests := []struct {
		name     string
		detector string
		history  string
		burn     string
	}{
		{
			name:     "multi-series.txt",
			detector: "daily-seasonal.yaml",
			history:  "multi-series.json",
		},
		{
			// The same history and the same band, linked to an SLO: two of
			// the two pages landed on a budget that was being spent.
			name:     "budget-overlap.txt",
			detector: "linked.yaml",
			history:  "multi-series.json",
			burn:     "two-of-three.json",
		},
		{
			// The report this number exists to produce: everything fired and
			// none of it coincided with anything.
			name:     "budget-never-burned.txt",
			detector: "linked.yaml",
			history:  "multi-series.json",
			burn:     "never.json",
		},
		{
			// The zero that is not a finding, said differently so that nobody
			// deletes a detector over a burn query that answered nothing.
			name:     "budget-unmeasured.txt",
			detector: "linked.yaml",
			history:  "multi-series.json",
			burn:     "no-series.json",
		},
		{
			name:     "nothing-fired.txt",
			detector: "patient.yaml",
			history:  "multi-series.json",
		},
		{
			name:     "no-series.txt",
			detector: "daily-seasonal.yaml",
			history:  "no-series.json",
		},
		{
			name:     "cold-warmup.txt",
			detector: "daily-seasonal.yaml",
			history:  "late-start.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var report *Report
			if tt.burn == "" {
				report = analyse(t, tt.detector, tt.history)
			} else {
				report = analyse(t, tt.detector, tt.history, tt.burn)
			}
			var out bytes.Buffer
			if err := report.Write(&out); err != nil {
				t.Fatalf("writing the report: %v", err)
			}
			golden(t, tt.name, out.Bytes())
		})
	}
}

func TestDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{time.Minute, "1m"},
		{90 * time.Second, "1m30s"},
		{time.Hour, "1h"},
		// The case a naive trim of a trailing "0m" breaks.
		{2*time.Hour + 30*time.Minute, "2h30m"},
		{102 * time.Hour, "102h"},
		{21*24*time.Hour + time.Hour, "505h"},
	}

	for _, tt := range tests {
		if got := duration(tt.in); got != tt.want {
			t.Errorf("duration(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
