package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"

	"github.com/dennisme/feno/internal/buildinfo"
	"github.com/dennisme/feno/internal/prometheus"
	"github.com/dennisme/feno/internal/spec"
	"github.com/dennisme/feno/internal/suggest"
)

// Defaults for the range a profile is read over.
const (
	// defaultProfileWindow is how much history a profile reads, in the
	// duration shorthand the specifications are written in.
	//
	// Three weeks, because of the longest question asked of it: a weekly
	// shape is only measurable where the range holds the lag plus a period
	// of overlap to correlate across, so two weeks is the floor and a third
	// is what makes the answer about two repetitions rather than one.
	// Longer is not free. It is somebody's production monitoring being read,
	// and at the step below a fourth week crosses the points-per-series
	// limit and becomes a second request per SLI.
	defaultProfileWindow = "21d"

	// defaultProfileStep is the resolution a profile reads at.
	//
	// Five minutes rather than the minute a backtest uses, because none of
	// the questions a profile answers is about a minute: a daily
	// autocorrelation and a scale ratio are the same at either resolution,
	// and the finer one costs five times the points for it. The cost is
	// stated in the draft's header, because coverage is not scale-free: a
	// series with a gap every third minute profiles as fully covered here
	// and would be cold at a ruler's own interval.
	defaultProfileStep = 5 * time.Minute
)

// suggestCommand drafts a detector per SLI and writes it out for review.
func suggestCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var (
		fs        = flag.NewFlagSet("suggest", flag.ContinueOnError)
		selected  pathList
		tree      pathList
		headers   headerList
		url       = fs.String("url", "", "base URL of a Prometheus-compatible server to profile the series against")
		window    = fs.String("window", defaultProfileWindow, "how much history to profile, as OpenSLO duration shorthand")
		step      = fs.Duration("step", defaultProfileStep, "resolution the profile reads at")
		out       = fs.String("o", "", "directory to write one draft per SLI into; the drafts go to stdout when unset")
		force     = fs.Bool("force", false, "replace a draft that is already in the output directory")
		maxPoints = fs.Int("max-points", 0, "the server's points-per-series limit, if it is lower than Prometheus' 11000")
		timeout   = fs.Duration("request-timeout", prometheus.DefaultTimeout, timeoutUsage)
	)
	fs.Var(&selected, "from-sli", "a file or directory of SLIs to draft from; repeatable")
	fs.Var(&tree, "f", "a tree of OpenSLO documents a drafted sloRef is resolved against; the --from-sli paths when unset; repeatable")
	fs.Var(&headers, "header", "a header to send with every request, as name=value; repeatable")
	fs.SetOutput(stderr)
	fs.Usage = func() {
		printf(stderr, "usage: feno suggest --from-sli <path> --url <endpoint> [-o <directory>]\n\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(selected) == 0 || *url == "" {
		fs.Usage()
		return exitUsage
	}

	profileWindow, err := shorthand(*window)
	if err != nil {
		printf(stderr, "--window %s\n", err)
		return exitUsage
	}
	if *step <= 0 {
		printf(stderr, "--step %s: must be positive\n", *step)
		return exitUsage
	}

	sources, code := chosen(selected, tree, stderr)
	if code != exitOK {
		return code
	}

	// Where each draft is going is settled before anything is read. A run
	// that would refuse to replace the twentieth file has no business
	// reading three weeks of history for the first nineteen, and a run that
	// failed halfway would leave a directory nobody can tell apart from a
	// finished one.
	destinations, err := destinations(*out, *force, sources)
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
	client.Timeout = *timeout

	end := time.Now().UTC().Truncate(*step)
	start := end.Add(-profileWindow)
	version := buildinfo.Get().Version

	var drafted int
	for _, source := range sources {
		profile, err := suggest.Measure(ctx, client, source.Query, start, end, *step)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				printf(stderr, "suggest interrupted after %d of %d SLIs: the drafts above are the ones that ran\n",
					drafted, len(sources))
				return exitSkipped
			}
			// The same degradation the backtest has, for the same reason: a
			// run that could not reach the datasource has not found out
			// that nothing is worth drafting.
			if prometheus.Unreachable(err) {
				printf(stderr, "suggest skipped: %s is not reachable: %s\n", *url, err)
				return exitSkipped
			}
			printf(stderr, "SLI %q: %s\n", source.Name, err)
			return exitUsage
		}

		decision, err := suggest.Decide(profile)
		if err != nil {
			// A refusal is the outcome spec.md Part 8.2 names, so it is
			// reported with its measurement and the run carries on to the
			// next SLI.
			if errors.Is(err, suggest.ErrUnfit) || errors.Is(err, suggest.ErrNoSeries) {
				printf(stderr, "SLI %q not drafted: %s\n", source.Name, err)
				continue
			}
			printf(stderr, "SLI %q: %s\n", source.Name, err)
			return exitUsage
		}

		draft, err := suggest.Draft(source, decision, profile, version)
		if err != nil {
			printf(stderr, "%s\n", err)
			return exitUsage
		}
		if len(source.SLOs) > 1 {
			printf(stderr, "SLI %q: %d SLOs name it, so sloRef is unset in the draft: %s\n",
				source.Name, len(source.SLOs), strings.Join(source.SLOs, ", "))
		}

		path, writing := destinations[source.Name]
		if !writing {
			if drafted > 0 {
				printf(stdout, "---\n")
			}
			printf(stdout, "%s", draft)
			drafted++
			continue
		}
		// A draft is reviewed and committed beside the specifications it was
		// derived from, so it carries the permissions those carry.
		if err := os.WriteFile(path, draft, 0o644); err != nil { //nolint:gosec
			printf(stderr, "%s\n", err)
			return exitUsage
		}
		printf(stdout, "%s\n", path)
		drafted++
	}

	if drafted == 0 {
		printf(stderr, "nothing worth drafting in %s\n", strings.Join(selected, ", "))
		return exitFinding
	}
	return exitOK
}

// chosen is the series to draft from, one per SLI, with the SLOs that named
// each one resolved against the tree.
//
// Two sets of paths, because the two answer different questions. --from-sli
// says which SLIs to draft from, and is the one spec.md Part 6 writes. The
// tree is what an sloRef is resolved against, and it has to be separable:
// pointing --from-sli at one SLI file in a repository that keeps SLIs,
// SLOs and DataSources in sibling directories would otherwise see no SLO at
// all and draft a detector tied to nothing.
func chosen(selected, tree pathList, stderr io.Writer) ([]suggest.Source, int) {
	chosenBundle, err := spec.Load(selected...)
	if err != nil {
		printf(stderr, "%s\n", err)
		return nil, exitUsage
	}
	slis := suggest.SLIs(chosenBundle)
	if len(slis) == 0 {
		printf(stderr, "no SLI to draft from in %s\n", strings.Join(selected, ", "))
		return nil, exitUsage
	}

	resolved := chosenBundle
	if len(tree) > 0 {
		resolved, err = spec.Load(tree...)
		if err != nil {
			printf(stderr, "%s\n", err)
			return nil, exitUsage
		}
	}

	var sources []suggest.Source
	for _, document := range slis {
		source, err := suggest.NewSource(document.SLI, document.File,
			suggest.SLOsNaming(resolved, document.SLI.Metadata.Name))
		if err != nil {
			printf(stderr, "not drafted: %s\n", err)
			continue
		}
		sources = append(sources, source)
	}
	if len(sources) == 0 {
		printf(stderr, "no SLI in %s holds a series a band suits\n", strings.Join(selected, ", "))
		return nil, exitFinding
	}
	return sources, exitOK
}

// destinations is the file each draft is written to, keyed by SLI name, and
// is empty when the drafts go to stdout.
//
// A file already there is refused by name rather than replaced. A draft is
// reviewed and then edited, so overwriting one silently would throw away the
// review, which is the only part of this command a person does.
func destinations(dir string, force bool, sources []suggest.Source) (map[string]string, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec
		return nil, err
	}

	out := make(map[string]string, len(sources))
	for _, source := range sources {
		path := filepath.Join(dir, source.Name+".yaml")
		if !force {
			if _, err := os.Stat(path); err == nil {
				return nil, fmt.Errorf("%s already exists: pass --force to replace it", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		out[source.Name] = path
	}
	return out, nil
}

// shorthand reads a duration written the way the specifications write one,
// so that `--window 21d` and `--window 1w` work where Go's own parser has no
// unit for either.
func shorthand(text string) (time.Duration, error) {
	parsed, err := v1.ParseDurationShorthand(text)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", text, err)
	}
	if err := parsed.Validate(); err != nil {
		return 0, fmt.Errorf("%q: %w", text, err)
	}
	if parsed.Duration() <= 0 {
		return 0, fmt.Errorf("%q: must be positive", text)
	}
	return parsed.Duration(), nil
}
