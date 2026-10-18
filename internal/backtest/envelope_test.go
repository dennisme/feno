package backtest

import (
	"bytes"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/dennisme/feno/internal/detect"
	"github.com/dennisme/feno/internal/episode"
	"github.com/dennisme/feno/internal/prometheus"
)

// TestPooledVersusEnvelope measures the open question in internal/detect
// rather than arguing it.
//
// [detect.Spec] pools one window per seasonal offset into a single sample set
// and takes one centre and one scale from the pool. grafana/promql-anomaly-
// detection, which spec.md Part 9 points at as the proven reference, instead
// computes a band per lookback and takes the envelope with
// `max without(look_back)`. internal/rules' package comment records that the
// two differ and that pooling is what the representation does; neither
// document has a number attached, because until there was a backtest there
// was nothing that could produce one.
//
// This is the number. Both are run over the same history, which is a series
// where one of the three pooled days ran high over the hours the band reads
// from. Pooling folds that day into one centre and one scale, so the scale
// absorbs it and the band grows wide enough to hide a real spike. The envelope
// keeps the day separate: two tight bands and one high one, and a spike above
// all three still breaches. The result is in testdata/golden.
//
// The envelope is built here rather than in the package because it is a
// measurement and not a feature. Nothing in feno emits or evaluates an
// envelope today, and adding a second band shape to the production path to
// answer a question about which shape to keep is how a tool ends up carrying
// both forever.
func TestPooledVersusEnvelope(t *testing.T) {
	d := load(t, "daily-seasonal.yaml")
	w := window(t, d)
	result := history(t, "seasonal-shape.json", w)

	band, err := d.Band(w.Step)
	if err != nil {
		t.Fatalf("band: %v", err)
	}
	if len(band.Offsets) != 3 {
		t.Fatalf("the band pools %d offsets, want 3", len(band.Offsets))
	}
	if len(result.Series) != 1 {
		t.Fatalf("the fixture holds %d series, want 1", len(result.Series))
	}
	points := result.Series[0].Points
	instants := detect.Range{Start: w.Start, End: w.End, Step: w.Step}

	pooled, err := detect.Evaluate(band, instants, points)
	if err != nil {
		t.Fatalf("pooled: %v", err)
	}
	envelope, bands, err := evaluateEnvelope(band, instants, points)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}

	opts := episode.Options{Step: w.Step, For: d.Spec.Alerting.For.Duration()}
	var out bytes.Buffer

	fmt.Fprintf(&out, "history             seasonal-shape.json, %d samples at %s\n",
		len(points), duration(w.Step))
	fmt.Fprintf(&out, "detector            %s: %s\n", d.Metadata.Name, describe(band))
	fmt.Fprintf(&out, "reported window     %s .. %s  (%d instants)\n",
		instant(w.Start), instant(w.End), w.Instants())
	fmt.Fprintf(&out, "hold duration       %s\n\n", duration(opts.For))

	rows := []struct {
		label   string
		results []detect.Result
	}{
		{"pooled", pooled},
		{"envelope", envelope},
	}
	fmt.Fprintf(&out, "%-24s%10s%10s\n", "", rows[0].label, rows[1].label)

	numbers := make([][]string, 0, 5)
	for _, row := range rows {
		episodes, err := episode.Extract(detect.Samples(row.results), opts)
		if err != nil {
			t.Fatalf("%s episodes: %v", row.label, err)
		}
		summary := episode.Summarize(episodes)
		counts := count(row.results)

		var breaching int
		for _, r := range row.results {
			if r.Breaching {
				breaching++
			}
		}
		numbers = append(numbers, []string{
			fmt.Sprintf("%d", summary.Firing),
			fmt.Sprintf("%d", summary.Flaps),
			duration(summary.FiringTime),
			fmt.Sprintf("%d", breaching),
			fmt.Sprintf("%d", counts.Evaluated),
		})
	}

	for i, label := range []string{
		"paging episodes", "flaps", "firing time",
		"breaching instants", "instants with a verdict",
	} {
		fmt.Fprintf(&out, "%-24s%10s%10s\n", label, numbers[0][i], numbers[1][i])
	}

	// The instant the two disagree about, spelled out, because the numbers
	// above say that they differ and not why.
	at, ok := disagreement(pooled, envelope)
	if !ok {
		t.Fatal("the two forms agree at every instant, so this fixture no longer measures anything")
	}
	fmt.Fprintf(&out, "\nthe first instant they disagree about, %s, sample %s\n",
		instant(pooled[at].At), number(pooled[at].Value))
	fmt.Fprintf(&out, "  pooled    [%s, %s]  centre %s, scale %s, breaching %v\n",
		number(pooled[at].Band.Lower()), number(pooled[at].Band.Upper()),
		number(pooled[at].Band.Centre), number(pooled[at].Band.Scale), pooled[at].Breaching)
	fmt.Fprintf(&out, "  envelope  [%s, %s]  breaching %v, from\n",
		number(envelope[at].Band.Lower()), number(envelope[at].Band.Upper()), envelope[at].Breaching)
	for i, offset := range band.Offsets {
		b := bands[at][i]
		fmt.Fprintf(&out, "    offset %-6s[%s, %s]  centre %s, scale %s\n",
			duration(offset), number(b.Lower()), number(b.Upper()),
			number(b.Centre), number(b.Scale))
	}

	out.WriteString("\nPooling spreads one unusual day across the scale, which widens the band\n")
	out.WriteString("far enough to swallow the spike. The envelope keeps the day in its own\n")
	out.WriteString("band, so the spike is still outside all three. This is one fixture and\n")
	out.WriteString("one shape of history, so it is evidence and not a decision: it says the\n")
	out.WriteString("two forms differ on the case seasonality exists for, which is more than\n")
	out.WriteString("was known before there was a backtest to run them through.\n")

	golden(t, "pooled-vs-envelope.txt", out.Bytes())
}

// evaluateEnvelope evaluates one band per offset and takes the widest, which
// is what `max without(look_back)` does in the Grafana reference.
//
// A value breaches only if it is outside every per-offset band, and an instant
// carries a verdict only if every offset did: a value judged against two of
// three seasonal baselines is not the same judgement, and treating it as one
// would flatter whichever form had the more missing history.
func evaluateEnvelope(
	pooledSpec detect.Spec, r detect.Range, points []prometheus.Point,
) ([]detect.Result, [][]detect.Band, error) {
	if len(pooledSpec.Offsets) == 0 {
		return nil, nil, fmt.Errorf("the band pools no offsets, so there is no envelope to take")
	}

	perOffset := make([][]detect.Result, 0, len(pooledSpec.Offsets))
	for _, offset := range pooledSpec.Offsets {
		spec := pooledSpec
		spec.Offsets = []time.Duration{offset}
		results, err := detect.Evaluate(spec, r, points)
		if err != nil {
			return nil, nil, fmt.Errorf("offset %v: %w", offset, err)
		}
		perOffset = append(perOffset, results)
	}

	out := make([]detect.Result, len(perOffset[0]))
	bands := make([][]detect.Band, len(perOffset[0]))
	for i := range out {
		result := detect.Result{
			Sample: perOffset[0][i].Sample,
			Status: detect.Evaluated,
			Value:  perOffset[0][i].Value,
		}
		bands[i] = make([]detect.Band, 0, len(perOffset))

		breaches := true
		lower, upper := math.Inf(1), math.Inf(-1)
		for _, results := range perOffset {
			r := results[i]
			bands[i] = append(bands[i], r.Band)
			if r.Status != detect.Evaluated {
				result.Status = r.Status
			}
			if !r.Breaching {
				breaches = false
			}
			lower, upper = math.Min(lower, r.Band.Lower()), math.Max(upper, r.Band.Upper())
		}

		// The envelope's edges are the widest pair, which belongs to no one
		// band, so they are carried as the centre and half width that
		// reproduce them.
		result.Band = detect.Band{Centre: (lower + upper) / 2, Scale: (upper - lower) / 2, Margin: 1}
		result.Breaching = breaches && result.Status == detect.Evaluated
		out[i] = result
	}
	return out, bands, nil
}

// disagreement is the first instant the two forms reach different verdicts.
func disagreement(a, b []detect.Result) (int, bool) {
	for i := range a {
		if a[i].Breaching != b[i].Breaching {
			return i, true
		}
	}
	return 0, false
}

// number formats a float for the comparison, at a fixed two places so the
// columns line up and a scale of 14.17 does not print fifteen digits.
func number(v float64) string { return fmt.Sprintf("%.2f", v) }
