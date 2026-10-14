package backtest

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dennisme/feno/internal/detect"
)

// labelWidth is how much room the label column of the summary block gets. It
// is fixed rather than computed so that two detectors in the same run print
// their numbers in the same place, which is what makes the two blocks
// comparable by eye.
const labelWidth = 20

// Write renders the report.
//
// The format is a golden file in testdata, because the thing being tested is
// not that the numbers are right, which the cases above cover, but that a
// person reading the output can tell which window a ratio is over and which
// series caused what. That is a property of the text.
func (r *Report) Write(w io.Writer) error {
	var b strings.Builder

	fmt.Fprintf(&b, "%s  %s\n", r.Detector, describe(r.Band))
	field(&b, "query", r.Query)
	field(&b, "step", fmt.Sprintf("%s, for %s", duration(r.Window.Step), duration(r.For)))
	field(&b, "requires", r.Requirements())
	field(&b, "reported window", fmt.Sprintf("%s .. %s  (%s, %d instants)",
		instant(r.Window.Start), instant(r.Window.End),
		duration(r.Window.Reported()), r.Window.Instants()))
	field(&b, "history read", fmt.Sprintf("%s .. %s  (%s, warm-up %s)",
		instant(r.Window.ReadStart), instant(r.Window.End),
		duration(r.Window.Read()), duration(r.Window.WarmUp())))
	field(&b, "read cost", fmt.Sprintf("%s, %d points, %s",
		plural(r.Stats.Requests, "request"), r.Stats.Points, plural(len(r.Series), "series")))
	for _, warning := range r.Stats.Warnings {
		field(&b, "server warning", warning)
	}

	b.WriteString("\n")
	if len(r.Series) == 0 {
		b.WriteString("  the query returned no series over the window read, so there is no\n")
		b.WriteString("  verdict here: this is the absence of data rather than a quiet detector\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	field(&b, "paging episodes", fmt.Sprintf("%d, plus %d flaps that crossed the band without reaching for",
		r.Episodes(), r.Flaps()))
	field(&b, "series firing time", fmt.Sprintf("%s  (%s of the reported window, summed over series)",
		duration(r.SeriesFiringTime()), percent(r.Ratio(r.SeriesFiringTime()))))
	field(&b, "paged wall clock", fmt.Sprintf("%s  (%s of the reported window)",
		duration(r.PagedTime()), percent(r.Ratio(r.PagedTime()))))

	if r.Peak == 0 {
		field(&b, "peak concurrent", "nothing fired")
	} else {
		field(&b, "peak concurrent", fmt.Sprintf("%s firing at %s",
			plural(r.Peak, "series"), instant(r.PeakAt)))
	}

	if where, longest, ok := r.Longest(); ok {
		field(&b, "longest episode", fmt.Sprintf("%s from %s  %s",
			duration(longest.FiringDuration()), instant(longest.FiredAt), labels(where.Labels)))
	} else {
		field(&b, "longest episode", "none")
	}

	// An instant with no verdict cannot breach, so a detector with nothing
	// firing and a third of its window cold has not been shown to be quiet.
	// The count is of series-instants, because that is what it is: every
	// series is evaluated at every instant.
	totals := r.Totals()
	field(&b, "without a verdict", fmt.Sprintf("%d of %d series-instants: %d cold, %d absent, %d undefined",
		totals.Cold+totals.Absent+totals.Undefined,
		r.Window.Instants()*len(r.Series),
		totals.Cold, totals.Absent, totals.Undefined))

	overlap(&b, r)

	b.WriteString("\n")
	if err := table(&b, r); err != nil {
		return err
	}

	b.WriteString("\n")
	b.WriteString("  Every ratio above divides by the reported window. Dividing by the span\n")
	b.WriteString("  the samples cover, or by the window times the series count, reads lower\n")
	b.WriteString("  and flatters the detector: the first hides a series that was missing,\n")
	b.WriteString("  the second shrinks every time the query matches another instance.\n")
	if r.Budget != nil {
		b.WriteString("\n")
		b.WriteString("  The budget overlap is the one exception: it divides by the time somebody\n")
		b.WriteString("  was paged, because the question behind it is what fraction of the pages\n")
		b.WriteString("  landed on an error budget that was actually being spent.\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// overlap is the budget overlap block: how much of the paging coincided with
// the linked SLO's error budget burning, and what that signal was.
//
// Each case prints a different sentence rather than a number, because the
// zeroes mean different things and the whole value of this block is that they
// are told apart. No link is a detector nobody has tied to an objective, an
// unmeasured budget is a query that answered nothing, and an overlap of zero
// over a budget that was read is the finding: a detector whose pages never
// landed on anything real.
func overlap(b *strings.Builder, r *Report) {
	if r.Budget == nil {
		field(b, "budget overlap", "no linked SLO: set spec.sloRef to the SLO this detector")
		field(b, "", "protects, or delete a detector nobody can tie to a burn")
		return
	}

	touched, episodes := r.TouchedEpisodes()
	switch {
	case !r.Budget.Measured():
		field(b, "budget overlap", "not computed: the burn query answered at no instant of the")
		field(b, "", "window, which is the absence of a budget rather than a quiet one")
	case episodes == 0:
		field(b, "budget overlap", "nothing fired, so nothing coincided with the budget burning")
	default:
		field(b, "budget overlap", fmt.Sprintf("%d of %d paging episodes coincided with the budget burning",
			touched, episodes))
		field(b, "", fmt.Sprintf("%s of %s paged wall clock  (%s)",
			duration(r.CoincidentTime()), duration(r.PagedTime()), percent(r.CoincidentRatio())))
	}

	field(b, "linked SLO", fmt.Sprintf("%s, objective %s", r.Budget.SLO, r.Budget.Objective))
	field(b, "budget burning", fmt.Sprintf("%d of %d instants: %d absent, %d undefined",
		r.Budget.Counts.Burning, r.Window.Instants(),
		r.Budget.Counts.Absent, r.Budget.Counts.Undefined))
	field(b, "burn signal", r.Budget.Query+" "+r.Budget.Comparison())
}

// table is the per-series breakdown, worst first, which is what answers
// "which series caused what".
func table(b *strings.Builder, r *Report) error {
	// A tabwriter buffers every row to measure the columns, so a write to it
	// cannot fail on its own: Flush below is where the underlying write
	// happens and is the error worth reporting.
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	row := func(format string, args ...any) { _, _ = fmt.Fprintf(tw, format, args...) }

	// The coincident column exists only when there is a budget to be
	// coincident with. A column of zeroes against no linked SLO would read as
	// a verdict, which is the mistake the block above is written to avoid.
	coincident := ""
	if r.Budget != nil {
		coincident = "\tcoincident"
	}

	row("  series\tfiring\tflaps\tfiring time\tratio\tcold\tabsent\tundefined%s\n", coincident)
	for _, s := range r.Series {
		if r.Budget != nil {
			coincident = "\t" + duration(s.Coincident())
		}
		row("  %s\t%d\t%d\t%s\t%s\t%d\t%d\t%d%s\n",
			labels(s.Labels), s.Summary.Firing, s.Summary.Flaps,
			duration(s.Summary.FiringTime), percent(r.Ratio(s.Summary.FiringTime)),
			s.Counts.Cold, s.Counts.Absent, s.Counts.Undefined, coincident)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing the series table: %w", err)
	}
	return nil
}

// field writes one label and value of the summary block. An empty name is a
// continuation line, which is how a value too long for one line is indented
// under the one above it.
func field(b *strings.Builder, name, value string) {
	fmt.Fprintf(b, "  %-*s  %s\n", labelWidth, name, value)
}

// describe is the band in the terms the author wrote it in, so that a report
// read on its own says which band produced it.
func describe(band detect.Spec) string {
	parts := []string{string(band.Strategy)}
	if band.Strategy == detect.Robust {
		parts = append(parts, string(band.RobustScale))
	}
	parts = append(parts, string(band.Sensitivity),
		fmt.Sprintf("window %s", duration(band.Window)))
	if n := len(band.Offsets); n > 0 {
		parts = append(parts, fmt.Sprintf("%d pooled offsets", n))
	}
	parts = append(parts, fmt.Sprintf("%d samples minimum", band.MinSamples))
	return strings.Join(parts, ", ")
}

// labels renders a label set the way a person reads one. The identity
// downstream is still prometheus.SeriesKey, which quotes both halves of every
// pair; this quotes only the value, because a report is read and not parsed.
func labels(set map[string]string) string {
	if len(set) == 0 {
		return "{}"
	}
	pairs := make([]string, 0, len(set))
	for _, name := range slices.Sorted(maps.Keys(set)) {
		pairs = append(pairs, name+"="+strconv.Quote(set[name]))
	}
	return "{" + strings.Join(pairs, ", ") + "}"
}

// instant formats a timestamp the way the rest of the tool prints one.
func instant(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// percent formats a fraction, with enough places that an hour in a month does
// not read as zero.
func percent(f float64) string { return strconv.FormatFloat(f*100, 'f', 2, 64) + "%" }

// plural is a count and its noun, so a report does not say "1 requests".
// Series is already plural, so it is left alone.
func plural(n int, noun string) string {
	if n == 1 || noun == "series" {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// duration drops the components a Duration prints as zero, so an episode
// reads as 4h rather than 4h0m0s. Nothing is rounded: a report that smoothed
// a 59 second episode into a minute would disagree with the step it was
// evaluated at.
func duration(d time.Duration) string {
	text := d.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}
