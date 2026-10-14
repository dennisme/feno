package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dennisme/feno/internal/prometheus"
	"github.com/dennisme/feno/internal/rules"
	"github.com/dennisme/feno/internal/spec"
)

// prometheusTarget is the only target that exists. ClickHouse is spec.md
// Part 12 and is not here, so the flag refuses anything else by name rather
// than compiling a specification for an engine nobody wired up.
const prometheusTarget = "prometheus"

// generateCommand compiles detectors into rule files, or checks the files
// already committed against what the specifications say they should be.
func generateCommand(args []string, stdout, stderr io.Writer) int {
	var (
		fs           = flag.NewFlagSet("generate", flag.ContinueOnError)
		paths        pathList
		headers      headerList
		engine       = fs.String("t", prometheusTarget, "the engine the rules are for")
		url          = fs.String("url", "", "base URL of the server the rules will run on, probed once for the optional functions it has")
		out          = fs.String("o", "", "directory the rule files are written to, and compared against under --check")
		step         = fs.Duration("step", defaultStep, "the interval the rules will be evaluated at, which the bands are computed for")
		check        = fs.Bool("check", false, "write nothing and fail if what is in the output directory is not what these specifications compile to")
		experimental = fs.Bool("experimental-functions", false,
			"the server was started with --enable-feature=promql-experimental-functions, which is what gates mad_over_time")
	)
	fs.Var(&paths, "f", "a file or directory of specifications; repeatable")
	fs.Var(&headers, "header", "a header to send with the probe, as name=value; repeatable")
	fs.SetOutput(stderr)
	fs.Usage = func() {
		printf(stderr, "usage: feno generate -f <path> -o <directory> [--check]\n\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if len(paths) == 0 || *out == "" {
		fs.Usage()
		return exitUsage
	}
	if *engine != prometheusTarget {
		printf(stderr, "-t %q: the only target is %q\n", *engine, prometheusTarget)
		return exitUsage
	}
	// Both flags answer the same question and the answers disagree by
	// construction, so preferring either one silently would be a flag that
	// does nothing.
	if given(fs, "url") && given(fs, "experimental-functions") {
		printf(stderr, "--url and --experimental-functions together: one asks the server whether it has mad_over_time, the other answers for it\n")
		return exitUsage
	}

	bundle, err := spec.Load(paths...)
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}
	if len(bundle.Detectors) == 0 {
		printf(stderr, "no detector to generate from in %s\n", strings.Join(paths, ", "))
		return exitUsage
	}

	target := rules.Target{ExperimentalFunctions: *experimental}
	if *url != "" {
		probed, err := probe(*url, http.Header(headers), stderr)
		if err != nil {
			printf(stderr, "%s\n", err)
			return exitUsage
		}
		target = probed
	}

	files, err := emit(target, *step, bundle.Detectors, stderr)
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}

	if *check {
		return verify(*out, files, stdout, stderr)
	}
	return write(*out, files, stdout, stderr)
}

// given reports whether a flag was named on the command line, which is the
// only way to tell a flag left at its default from one set to it.
func given(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// probe asks the server which form of the robust band it can evaluate.
//
// Only the server knows: the functions a band may use are gated by how the
// process was started, and nothing in a specification file can say. The
// decision is said out loud because it changes the rules that come out, and a
// rule that changed shape between two runs has to explain itself.
func probe(url string, header http.Header, stderr io.Writer) (rules.Target, error) {
	client, err := prometheus.New(url)
	if err != nil {
		return rules.Target{}, err
	}
	client.Header = header

	capabilities, err := client.Probe(context.Background())
	if err != nil {
		return rules.Target{}, err
	}

	form := "mad_over_time unavailable, emitting interquartile range"
	if capabilities.ExperimentalFunctions {
		form = "mad_over_time available, emitting median absolute deviation"
	}
	printf(stderr, "probed %s: %s\n", url, form)

	return rules.Target{ExperimentalFunctions: capabilities.ExperimentalFunctions}, nil
}

// file is one rule file: the name it takes in the output directory, and the
// bytes the specifications compiled to.
type file struct {
	name  string
	rules []byte
}

// emit compiles one rule file per specification file.
//
// One file in, one file out, because that is what makes the provenance header
// and the drift gate work: a reviewer reading a generated file has one
// specification to go and read, and a hash of it to tell a stale
// regeneration from a hand edit. A single bundled file would carry a list of
// hashes and say nothing about which rule came from which.
//
// Degradations are written as they are found rather than returned. Every one
// of them is also in the file it belongs to, so this is the copy a person
// watching the command run sees.
func emit(target rules.Target, step time.Duration, detectors []spec.Detector, stderr io.Writer) ([]file, error) {
	bySource := map[string][]spec.Detector{}
	for _, d := range detectors {
		bySource[d.File] = append(bySource[d.File], d)
	}

	sources := make([]string, 0, len(bySource))
	for source := range bySource {
		sources = append(sources, source)
	}
	slices.Sort(sources)

	files := make([]file, 0, len(sources))
	for _, source := range sources {
		specification, err := os.ReadFile(source) //nolint:gosec
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", source, err)
		}

		out, err := rules.Emit(target, step, rules.Source{
			Path:          source,
			Specification: specification,
		}, bySource[source]...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", source, err)
		}
		for _, degradation := range out.Degradations {
			printf(stderr, "%s: %s\n", source, degradation)
		}

		files = append(files, file{name: filepath.Base(source), rules: out.Rules})
	}
	return files, nil
}

// write puts the rule files in dir, creating it if it is not there.
func write(dir string, files []file, stdout, stderr io.Writer) int {
	// Rule files are committed and read by whatever ruler the team runs, so
	// they carry the permissions a configuration file in a repository carries
	// rather than a private one's.
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec
		printf(stderr, "%s\n", err)
		return exitUsage
	}
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, f.rules, 0o644); err != nil { //nolint:gosec
			printf(stderr, "%s\n", err)
			return exitUsage
		}
		printf(stdout, "%s\n", path)
	}
	return exitOK
}

// verify is the drift gate spec.md Part 10 asks for: it writes nothing and
// fails when the committed files are not what these specifications compile
// to. Three things are drift, and the message says which: a file that differs,
// a file that is missing, and a file nothing accounts for, which is what a
// deleted specification leaves behind.
func verify(dir string, files []file, stdout, stderr io.Writer) int {
	drifted := false

	for _, f := range files {
		path := filepath.Join(dir, f.name)
		committed, err := os.ReadFile(path) //nolint:gosec
		if err != nil {
			printf(stderr, "%s: %s\n", path, err)
			drifted = true
			continue
		}
		if line, differs := difference(committed, f.rules); differs {
			printf(stderr, "%s: line %d:\n  committed:  %s\n  generated:  %s\n",
				path, line.number, line.committed, line.generated)
			drifted = true
		}
	}

	expected := make([]string, 0, len(files))
	for _, f := range files {
		expected = append(expected, f.name)
	}
	extra, err := unaccounted(dir, expected)
	if err != nil {
		printf(stderr, "%s\n", err)
		return exitUsage
	}
	for _, name := range extra {
		printf(stderr, "%s: no specification produces this file\n", filepath.Join(dir, name))
		drifted = true
	}

	if drifted {
		printf(stderr, "run feno generate to regenerate\n")
		return exitFinding
	}
	printf(stdout, "%d rule files match their specifications\n", len(files))
	return exitOK
}

// line is the first place two rule files stop agreeing.
type line struct {
	number    int
	committed string
	generated string
}

// difference reports the first line the two files disagree on.
//
// The first line is enough to act on and keeps the gate's output short: the
// fix is always the same command, and the full diff is a `git diff` away once
// it has run. A changed hash under an unchanged body says a specification
// moved on, and a changed body under an unchanged hash says somebody edited
// the generated file.
func difference(committed, generated []byte) (line, bool) {
	if string(committed) == string(generated) {
		return line{}, false
	}

	was := strings.Split(string(committed), "\n")
	is := strings.Split(string(generated), "\n")
	for i := range max(len(was), len(is)) {
		l := line{number: i + 1}
		if i < len(was) {
			l.committed = was[i]
		}
		if i < len(is) {
			l.generated = is[i]
		}
		if l.committed != l.generated {
			return l, true
		}
	}
	return line{}, false
}

// unaccounted is every YAML file in dir that no specification produced.
//
// Only YAML, and only this directory: the output sits next to whatever sloth
// or pyrra generates, and a gate that failed on a README or on a
// subdirectory would be a gate people turn off.
func unaccounted(dir string, expected []string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var extra []string
	for _, entry := range entries {
		if entry.IsDir() || !isYAML(entry.Name()) || slices.Contains(expected, entry.Name()) {
			continue
		}
		extra = append(extra, entry.Name())
	}
	return extra, nil
}

// isYAML is the extensions a rule file is written with.
func isYAML(name string) bool {
	switch filepath.Ext(name) {
	case ".yaml", ".yml":
		return true
	}
	return false
}
