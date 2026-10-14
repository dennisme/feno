package backtest

import (
	"errors"
	"testing"
	"time"

	"github.com/dennisme/feno/internal/prometheus"
	"github.com/dennisme/feno/internal/rules"
)

// TestBudgetOverlap is the number the report exists for: how much of a
// detector's paging coincided with the error budget of the SLO it links to
// actually burning.
//
// The arithmetic is all over the same twenty-five hour grid. multi-series.json
// pages for three hours, 11:00 to 14:00, with web-2 firing all three and
// web-1 the first two of them. two-of-three.json burns at 12:00 and 13:00, and
// again at 20:00 when nothing was firing. So:
//
//	web-2 fires 11, 12, 13   and coincides at 12, 13   two hours
//	web-1 fires 11, 12       and coincides at 12       one hour
//	web-3 never fires        and coincides nowhere     none
//
// and the wall clock, which counts the two instances firing at 12:00 once, is
// two of the three paged hours.
func TestBudgetOverlap(t *testing.T) {
	report := analyse(t, "linked.yaml", "multi-series.json", "two-of-three.json")

	if report.Budget == nil {
		t.Fatal("a detector linked to an SLO reported no budget")
	}
	if got := report.Budget.SLO; got != "checkout-availability" {
		t.Errorf("SLO = %q, want %q", got, "checkout-availability")
	}
	if got := report.Budget.Objective; got != "99.9%" {
		t.Errorf("objective = %q, want %q", got, "99.9%")
	}

	// Three burning instants of twenty-five. One instant carried no sample
	// and one carried a value the server could not compute, and neither is a
	// quiet budget, so both are counted apart from the instants that were
	// measured and found quiet.
	want := BurnCounts{Evaluated: 23, Absent: 1, Undefined: 1, Burning: 3}
	if report.Budget.Counts != want {
		t.Errorf("burn signal = %+v, want %+v", report.Budget.Counts, want)
	}

	coincident := map[string]time.Duration{"web-2": 2 * time.Hour, "web-1": time.Hour, "web-3": 0}
	for _, series := range report.Series {
		instance := series.Labels["instance"]
		if got := series.Coincident(); got != coincident[instance] {
			t.Errorf("%s coincided for %s, want %s", instance, got, coincident[instance])
		}
	}

	touched, episodes := report.TouchedEpisodes()
	if touched != 2 || episodes != 2 {
		t.Errorf("touched episodes = %d of %d, want 2 of 2", touched, episodes)
	}
	if got := report.CoincidentTime(); got != 2*time.Hour {
		t.Errorf("coincident wall clock = %s, want 2h", got)
	}
	// The denominator is the time somebody was paged, not the window: the
	// question is what fraction of the pages landed on something real.
	if got := report.CoincidentRatio(); got != 2.0/3.0 {
		t.Errorf("coincident ratio = %v, want %v", got, 2.0/3.0)
	}
}

// TestBudgetNeverBurned is the deletion case stated as numbers: everything
// fired, nothing coincided, and the budget signal was read over every instant
// so the zero is a finding rather than a gap.
func TestBudgetNeverBurned(t *testing.T) {
	report := analyse(t, "linked.yaml", "multi-series.json", "never.json")

	touched, episodes := report.TouchedEpisodes()
	if touched != 0 || episodes != 2 {
		t.Errorf("touched episodes = %d of %d, want 0 of 2", touched, episodes)
	}
	if got := report.CoincidentTime(); got != 0 {
		t.Errorf("coincident wall clock = %s, want 0s", got)
	}
	want := BurnCounts{Evaluated: 25}
	if report.Budget.Counts != want {
		t.Errorf("burn signal = %+v, want %+v", report.Budget.Counts, want)
	}
	if !report.Budget.Measured() {
		t.Error("a burn signal read over every instant reports itself unmeasured")
	}
}

// TestBudgetNotMeasured is the other zero, and the reason the two are kept
// apart: a burn query that matched nothing says nothing about the detector.
func TestBudgetNotMeasured(t *testing.T) {
	report := analyse(t, "linked.yaml", "multi-series.json", "no-series.json")

	if report.Budget == nil {
		t.Fatal("a detector linked to an SLO reported no budget")
	}
	if report.Budget.Measured() {
		t.Error("a burn query that returned no series reports itself measured")
	}
	want := BurnCounts{Absent: 25}
	if report.Budget.Counts != want {
		t.Errorf("burn signal = %+v, want %+v", report.Budget.Counts, want)
	}
}

// TestBudgetUnlinked is the detector nobody tied to an objective. It reports,
// and it reports no budget rather than an overlap of zero.
func TestBudgetUnlinked(t *testing.T) {
	report := analyse(t, "daily-seasonal.yaml", "multi-series.json")

	if report.Budget != nil {
		t.Errorf("an unlinked detector reported a budget: %+v", report.Budget)
	}
	if touched, episodes := report.TouchedEpisodes(); touched != 0 || episodes != 2 {
		t.Errorf("touched episodes = %d of %d, want 0 of 2", touched, episodes)
	}
}

// TestBudgetErrors are the reads that cannot be intersected. Each is refused,
// because every one of them would otherwise report an overlap of zero, which
// is the one conclusion this number is read for.
func TestBudgetErrors(t *testing.T) {
	linked := load(t, "linked.yaml")
	unlinked := load(t, "daily-seasonal.yaml")
	w := window(t, linked)

	tests := []struct {
		name string
		read func() (*Report, error)
		want error
	}{
		{
			name: "a linked detector with no burn signal read",
			read: func() (*Report, error) {
				return Analyse(linked, w, history(t, "multi-series.json", w), nil)
			},
			want: ErrBurnRead,
		},
		{
			name: "an unlinked detector handed one",
			read: func() (*Report, error) {
				return Analyse(unlinked, w, history(t, "multi-series.json", w), burn(t, "never.json", w))
			},
			want: ErrBurnRead,
		},
		{
			name: "a burn query that returned more than one series",
			read: func() (*Report, error) {
				result := burn(t, "never.json", w)
				second := result.Series[0]
				second.Key = `{slo="other"}`
				second.Labels = map[string]string{"slo": "other"}
				result.Series = append(result.Series, second)
				return Analyse(linked, w, history(t, "multi-series.json", w), result)
			},
			want: ErrBurnSeries,
		},
		{
			name: "a burn signal read off the grid the detector was evaluated on",
			read: func() (*Report, error) {
				result := burn(t, "never.json", w)
				for i := range result.Series[0].Points {
					result.Series[0].Points[i].At = result.Series[0].Points[i].At.Add(30 * time.Second)
				}
				return Analyse(linked, w, history(t, "multi-series.json", w), result)
			},
			want: ErrBurnGrid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := tt.read()
			if err == nil {
				t.Fatalf("Analyse succeeded, want %v: %+v", tt.want, report.Budget)
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

// TestBudgetRefusesAnUncompilableSLO is a link to an objective no burn signal
// can be compiled from. It fails the backtest rather than reporting an
// overlap the reader would read as a verdict.
func TestBudgetRefusesAnUncompilableSLO(t *testing.T) {
	d := load(t, "linked.yaml")
	w := window(t, d)

	// Two objectives are two budgets burning at two rates, which
	// internal/rules refuses to pick between.
	d.SLO.Spec.Objectives = append(d.SLO.Spec.Objectives, d.SLO.Spec.Objectives[0])

	_, err := Analyse(d, w, history(t, "multi-series.json", w), &prometheus.Result{})
	if !errors.Is(err, rules.ErrObjectives) {
		t.Fatalf("got %v, want %v", err, rules.ErrObjectives)
	}
}
