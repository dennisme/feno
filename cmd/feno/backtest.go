package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"

	"github.com/dennisme/feno/internal/backtest"
	"github.com/dennisme/feno/internal/prometheus"
	"github.com/dennisme/feno/internal/spec"
)

// defaultStep is the resolution a backtest reads at when nobody says.
//
// A minute, because that is what a Prometheus evaluates rules at unless its
// operator changed it, and the step is what the report's numbers predict: a
// backtest at five minutes predicts a rule evaluated at five minutes and no
// other. Somebody whose ruler runs at a different interval passes it.
const defaultStep = time.Minute

// backtestCommand replays detectors over history and prints the report.
func backtestCommand(args []string, stdout, stderr io.Writer) int {
	var (
		fs         = flag.NewFlagSet("backtest", flag.ContinueOnError)
		paths      pathList
		headers    headerList
		url        = fs.String("url", "", "base URL of a Prometheus-compatible server, such as http://mimir/prometheus")
		from       = fs.String("from", "", "start of the window to report on, as -30d or an RFC 3339 timestamp")
		to         = fs.String("to", "", "end of the window to report on; now when unset")
		step       = fs.Duration("step", defaultStep, "query resolution, and the evaluation interval the report predicts")
		only       = fs.String("detector", "", "report on one detector by name rather than every one loaded")
		maxPoints  = fs.Int("max-points", 0, "the server's points-per-series limit, if it is lower than Prometheus' 11000")
		maxEpisode = fs.Int("max-episodes", -1, "fail if a detector would have paged more than this many times")
		maxRatio   = fs.Float64("max-firing-ratio", -1, "fail if a detector would have been firing for more than this fraction of the window")
	)
	fs.Var(&paths, "f", "a file or directory of specifications; repeatable")
	fs.Var(&headers, "header", "a header to send with every request, as name=value; repeatable")
	fs.SetOutput(stderr)
	fs.Usage = func() {
		printf(stderr, "usage: feno backtest -f <path> --url <endpoint> --from -30d\n\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(paths) == 0 || *url == "" || *from == "" {
		fs.Usage()
		return exitUsage
	}

	end := time.Now().UTC()
	if *to != "" {
		parsed, err := timestamp(*to, time.Now().UTC())
		if err != nil {
			printf(stderr, "--to %s\n", err)
			return exitUsage
		}
		end = parsed
	}
	// The grid is anchored on round instants, so two runs a few seconds apart
	// read the same instants and their reports can be compared.
	end = end.Truncate(*step)

	start, err := timestamp(*from, end)
	if err != nil {
		printf(stderr, "--from %s\n", err)
		return exitUsage
	}

	bundle, err := spec.Load(paths...)
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}

	client, err := prometheus.New(*url)
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}
	client.Header = http.Header(headers)
	client.MaxPointsPerSeries = *maxPoints

	options := backtest.Options{From: start, To: end, Step: *step}
	thresholds := backtest.Thresholds{MaxEpisodes: *maxEpisode, MaxFiringRatio: *maxRatio}

	var (
		reported int
		findings []string
	)
	for _, detector := range bundle.Detectors {
		if *only != "" && detector.Metadata.Name != *only {
			continue
		}

		report, err := backtest.Run(context.Background(), client, detector, options)
		if err != nil {
			printf(stderr, "%s\n", err)
			return exitUsage
		}
		if reported > 0 {
			printf(stdout, "\n")
		}
		if err := report.Write(stdout); err != nil {
			printf(stderr, "%s\n", err)
			return exitUsage
		}
		reported++
		findings = append(findings, report.Exceeded(thresholds)...)
	}

	if reported == 0 {
		printf(stderr, "no detector to report on in %s\n", strings.Join(paths, ", "))
		return exitUsage
	}
	if len(findings) > 0 {
		for _, finding := range findings {
			printf(stderr, "%s\n", finding)
		}
		return exitFinding
	}
	return exitOK
}

// timestamp reads a flag that is either an RFC 3339 instant or a duration
// before now, written with a leading minus.
//
// The relative form uses OpenSLO's duration shorthand, which is what the
// specifications already say `window: 1h` and `for: 10m` in, rather than Go's
// parser: a backtest is asked for in days and weeks, and time.ParseDuration
// has no unit for either.
func timestamp(text string, now time.Time) (time.Time, error) {
	if !strings.HasPrefix(text, "-") {
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q is neither an RFC 3339 timestamp nor a duration like -30d", text)
		}
		return parsed.UTC(), nil
	}

	shorthand, err := v1.ParseDurationShorthand(strings.TrimPrefix(text, "-"))
	if err != nil {
		return time.Time{}, fmt.Errorf("%q: %w", text, err)
	}
	if err := shorthand.Validate(); err != nil {
		return time.Time{}, fmt.Errorf("%q: %w", text, err)
	}
	return now.Add(-shorthand.Duration()), nil
}

// pathList collects a repeated -f.
type pathList []string

func (p *pathList) String() string { return strings.Join(*p, ", ") }

func (p *pathList) Set(value string) error {
	if value == "" {
		return fmt.Errorf("a path must not be empty")
	}
	*p = append(*p, value)
	return nil
}

// headerList collects a repeated --header, which is how a multi-tenant target
// is reached at all: Mimir and Thanos want X-Scope-OrgID, and a proxy in front
// of any of them wants whatever it wants.
type headerList http.Header

func (h *headerList) String() string {
	names := make([]string, 0, len(*h))
	for name := range *h {
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

func (h *headerList) Set(value string) error {
	name, content, found := strings.Cut(value, "=")
	if !found || name == "" {
		return fmt.Errorf("%q is not name=value", value)
	}
	if *h == nil {
		*h = headerList{}
	}
	http.Header(*h).Add(name, content)
	return nil
}
