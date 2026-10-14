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

// metric is what the recipe passes as FENO_PROMETHEUS_QUERY, so the name here
// and the query there are one decision.
const metric = "feno_requests_per_second"

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

	if _, err := fmt.Fprintf(w, "# HELP %s Synthetic request rate with daily shape.\n# TYPE %s gauge\n", metric, metric); err != nil {
		return err
	}

	for i, service := range services {
		// Seeded per series index rather than from the clock, so two runs over
		// the same span differ only in where the span sits. Reproducibility is
		// the property wanted, which is what a weak generator gives.
		noise := rand.New(rand.NewPCG(uint64(i), 0)) //nolint:gosec
		for at := start; !at.After(end); at = at.Add(step) {
			value := rate(service, at, noise)
			if _, err := fmt.Fprintf(w, "%s{service=\"%s\"} %.3f %d\n", metric, service, value, at.Unix()); err != nil {
				return err
			}
		}
	}

	_, err := fmt.Fprint(w, "# EOF\n")
	return err
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
