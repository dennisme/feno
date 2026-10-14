// Command seed writes a span of OpenMetrics samples for promtool to import
// into TSDB blocks.
//
// It exists because the backtest integration test reads time.Now(): a
// committed block file would be outside the window asked for by the next day,
// and every instant would come back absent, which is the symptom this test is
// here to catch rather than the condition it should run under. So the history
// is generated at run time, ending at the minute the run starts.
//
// The series have daily shape on purpose. An adaptive band with daily
// seasonality fitted against a flat line computes a band of nothing, and a
// report of nothing is evidence the read happened, not that the read landed
// where the band was evaluated.
//
// A second family carries the successful half of the same traffic, so that an
// SLO over the pair has an error budget that is spent sometimes and not
// always. That is what the budget overlap needs to be evidence of anything:
// the overlap is an intersection of two range reads, and two reads on grids
// offset from each other intersect in nothing whatever the data did.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"time"
)

// The metric names the recipe passes as queries, so the names here and the
// queries there are one decision.
const (
	// requestMetric is the series a detector watches.
	requestMetric = "feno_requests_per_second"

	// goodMetric is the part of it that succeeded, which is the good half of
	// the SLO's ratio. The total half is requestMetric.
	goodMetric = "feno_good_per_second"
)

// metrics is every family written, with the line that describes it.
var metrics = []struct {
	name string
	help string
	good bool
}{
	{name: requestMetric, help: "Synthetic request rate with daily shape."},
	{name: goodMetric, help: "The part of the synthetic request rate that succeeded.", good: true},
}

// services are the label sets, one series each. More than one because the
// reader stitches a chunked range back together per label set, and a single
// series cannot be wrong about which samples belong to which.
var services = []string{"api", "checkout"}

func main() {
	span := flag.Duration("span", 35*24*time.Hour, "how much history to write, ending at -end")
	step := flag.Duration("step", time.Minute, "spacing between samples")
	end := flag.String("end", "", "last sample's timestamp, RFC3339; defaults to the current minute")
	flag.Parse()

	last := time.Now().UTC().Truncate(time.Minute)
	if *end != "" {
		parsed, err := time.Parse(time.RFC3339, *end)
		if err != nil {
			log.Fatalf("parsing -end: %v", err)
		}
		last = parsed.UTC()
	}

	out := bufio.NewWriter(os.Stdout)
	if err := emit(out, *span, *step, last); err != nil {
		log.Fatalf("writing samples: %v", err)
	}
	if err := out.Flush(); err != nil {
		log.Fatalf("writing samples: %v", err)
	}
}

// emit writes every series in full before starting the next, which is the
// order the OpenMetrics parser wants: one HELP and one TYPE for the family,
// then its samples.
func emit(w io.Writer, span, step time.Duration, end time.Time) error {
	start := end.Add(-span)

	for _, metric := range metrics {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n",
			metric.name, metric.help, metric.name); err != nil {
			return err
		}

		for i, service := range services {
			// Seeded per series index rather than from the clock, so two runs
			// over the same span differ only in where the span sits.
			// Reproducibility is the property wanted, which is what a weak
			// generator gives. The same seed per metric is deliberate: the
			// good half has to be the same traffic as the total half, or the
			// ratio between them is noise over noise.
			noise := rand.New(rand.NewPCG(uint64(i), 0)) //nolint:gosec
			for at := start; !at.After(end); at = at.Add(step) {
				value := rate(service, at, noise)
				if metric.good {
					value *= 1 - errorRatio(at)
				}
				if _, err := fmt.Fprintf(w, "%s{service=\"%s\"} %.3f %d\n",
					metric.name, service, value, at.Unix()); err != nil {
					return err
				}
			}
		}
	}

	_, err := fmt.Fprint(w, "# EOF\n")
	return err
}

// errorRatio is the fraction of requests that failed at an instant.
//
// Quiet at a tenth of what a 99.9% objective allows, with one bad hour every
// third day at twenty times the allowance. Both halves matter: a budget that
// burns all month and a budget that never burns are each a report in which an
// overlap of zero proves nothing about the detector.
func errorRatio(at time.Time) float64 {
	if at.Day()%3 == 0 && at.Hour() == 2 {
		return 0.02
	}
	return 0.0001
}

// rate is a daily sine around a baseline, lower at the weekend, with enough
// noise that a band fitted to it has a width.
func rate(service string, at time.Time, noise *rand.Rand) float64 {
	baseline := 100.0
	if service == "checkout" {
		baseline = 40.0
	}

	day := float64(at.Hour())*3600 + float64(at.Minute())*60 + float64(at.Second())
	daily := math.Sin(2 * math.Pi * day / 86400)

	weekend := 1.0
	if at.Weekday() == time.Saturday || at.Weekday() == time.Sunday {
		weekend = 0.6
	}

	return (baseline + 0.3*baseline*daily + 0.05*baseline*noise.NormFloat64()) * weekend
}
