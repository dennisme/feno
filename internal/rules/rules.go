// Package rules compiles a band into the recording and alerting rules a
// Prometheus-compatible server already evaluates.
//
// # What this does not do
//
// No band math. [detect.Spec] says what the band is and this turns that into
// PromQL, so nothing here computes a centre or a scale and nothing here
// carries its own copy of a sensitivity table or a scale conversion. Finding
// arithmetic in this package is the signal that the representation is
// missing a field, not an invitation to work around it.
//
// # Why pooled seasonality forces the rule shape it does
//
// [detect.Spec] pools one window per offset into a single sample set and
// takes one centre and one scale from the pool. PromQL has no way to express
// that directly: `quantile_over_time` reads one range selector, and there is
// no union of six disjoint ranges to take a median of.
//
// For the adaptive strategy the pool is still reachable exactly, because a
// mean and a population variance decompose. With N windows of equal length,
// the pooled mean is the mean of the per-window means, and the pooled
// variance is the mean of the per-window variances plus the variance of
// those means. Both terms are ordinary PromQL over N known offsets, so the
// generated rule computes the same number the backtest does rather than
// something close to it.
//
// For the robust strategy it is not reachable at all. A median of a pool is
// not a function of the per-window medians, so `robust` with any seasonality
// cannot be expressed on this target and [Emit] refuses it rather than
// emitting a median of medians that would look plausible and disagree with
// every backtest. spec.md Part 9 asked for exactly one deliberately
// uncompilable specification to exercise the degradation path while
// Prometheus is the only target; this is that case, and it was found rather
// than invented.
//
// # Where this departs from the Grafana reference
//
// grafana/promql-anomaly-detection is the proven rule library spec.md Part 9
// points at, and the structure here follows it: three series per detector
// carrying the level and the two band edges, a margin that floors the band's
// width, and the alert comparing the level against the edges. Four things
// are deliberately different, each because [detect.Spec] says so and the
// backtest evaluates [detect.Spec]:
//
//   - The reference computes a band per lookback and takes the envelope with
//     `max without(look_back)`. This pools, because that is what the
//     representation does.
//   - The reference offsets by a period minus half the window, `offset
//     167h30m` for a one hour window at a week, which centres the window on
//     the same time of day. Offsets here are the exact durations in
//     Spec.Offsets, because [detect.Spec.Lookback] and the backtest's query
//     range are computed from those.
//   - The reference's margin is proportional to the level, `median * 0.5`.
//     Spec.MinWidth is an absolute floor in the series' own units.
//   - The reference clamps the lower band at zero. Nothing here clamps,
//     because [detect.Band.Lower] does not.
//
// Keeping any of those four would make `generate` and `backtest` describe
// different bands, which is the one failure the single band implementation
// exists to prevent.
//
// # What the rule cannot say
//
// Spec.MinSamples has no expression here. A rule computes a band from
// whatever samples its range selector found and has no way to report that
// the window was a quarter full, so [detect.Cold] is a backtest-only answer.
// A generated rule over a window that is mostly empty produces a band from
// the few samples present, which is why the coverage policy is worth
// reading in a backtest report before the rules ship rather than after.
//
// The one place the rule does refuse is a window holding nothing at all:
// `avg_over_time` over an empty range returns no sample, so every rule
// downstream of it drops the series. That is why the floor internal/detect
// applies is a count per window rather than on the pool, and the parity
// test asserts both sides refusing there.
//
// # One group per detector, one interval
//
// Rules inside a Prometheus group evaluate in order and see the results that
// earlier rules in the same group wrote at the same timestamp. So the
// intermediates here are referenced directly, where the reference library
// wraps each one in `last_over_time(...[2m])` because it splits its bands
// across groups with different intervals. One group at one interval is the
// simpler shape and it removes a staleness alignment the backtest would
// otherwise have to reproduce. The cost is that seasonal offsets re-read
// their windows at the band's own interval rather than a slower one.
//
// # What a pooled seasonal band costs
//
// Two recording rules per offset, plus the level, the centre, the scale, the
// half width and the two edges. Daily and weekly together is six offsets, so
// nineteen recording rules for one detector, against the six to ten spec.md
// Part 4 budgets a band at. That number is a consequence of pooling rather
// than of this compiler: an envelope over per-offset bands, which is what
// the Grafana reference computes, needs one band per offset and no
// decomposition. Worth weighing against the pooling decision rather than
// absorbing quietly.
package rules

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dennisme/feno/internal/buildinfo"
	"github.com/dennisme/feno/internal/detect"
	"github.com/dennisme/feno/internal/spec"
)

var (
	// ErrUnsupported reports a band this target cannot express at all, as
	// opposed to one it expresses in a weaker form. The difference matters
	// to a caller: a degradation is a note in a report, and this is a
	// specification that has to change.
	ErrUnsupported = errors.New("not expressible in PromQL")

	// ErrName reports a detector whose name cannot be part of a Prometheus
	// metric name, or two that would compile to the same one.
	ErrName = errors.New("detector name")

	// ErrQuery reports a metric source carrying no PromQL query under the
	// key this target reads.
	ErrQuery = errors.New("no query for the Prometheus target")

	// ErrSourceType reports a detector pointed at something that does not
	// speak PromQL.
	ErrSourceType = errors.New("not a Prometheus datasource")
)

// SourceType is the OpenSLO datasource type this target compiles for.
const SourceType = "Prometheus"

// Target is what the server that will evaluate these rules can do.
//
// This is a property of the deployment and not of the dialect: the same
// specification against the same PromQL compiles differently depending on
// which experimental functions that particular server was started with. So
// it arrives as a value rather than being probed here. spec.md Part 11 puts
// the probe at generate time, one instant query per target, which needs an
// HTTP client and belongs with the reader rather than with the compiler.
type Target struct {
	// ExperimentalFunctions is whether the server was started with
	// --enable-feature=promql-experimental-functions, which is what gates
	// `mad_over_time`.
	ExperimentalFunctions bool
}

// Degradation is a specification this target compiled in a weaker form than
// it was written in.
//
// It is returned rather than logged or ignored, because a rule that quietly
// changed shape between two runs is the thing a person debugging an alert at
// three in the morning cannot see.
type Degradation struct {
	// Detector is the name as authored.
	Detector string

	// Requirement is what the band asked for and this target lacks.
	Requirement detect.Requirement

	// Substitute is what was emitted instead, in words.
	Substitute string
}

func (d Degradation) String() string {
	return fmt.Sprintf("%s: %s unavailable, emitted %s", d.Detector, d.Requirement, d.Substitute)
}

// Source is the specification file a rule file was generated from.
//
// It is what makes the header provenance rather than decoration: a reviewer
// reading forty lines of PromQL can find the specification change behind it,
// and the drift gate can tell a specification that was changed without
// regenerating from a generated file somebody edited by hand. The first
// shows up as a changed hash, the second as a changed body under an
// unchanged hash.
type Source struct {
	// Path is the specification file, as the invocation named it.
	Path string

	// Specification is that file's bytes. They are hashed into the header
	// and not otherwise read: the caller has already decoded them into the
	// detectors passed alongside.
	Specification []byte
}

// Output is a rule file and what it cost to produce.
type Output struct {
	// Rules is the file, provenance header included, ready to write next to
	// whatever sloth or pyrra already generates.
	Rules []byte

	// Degradations is every specification that compiled in a weaker form.
	// Empty means every band is the band that was written.
	Degradations []Degradation
}

// Emit compiles detectors into one rule file for target.
//
// step is the interval the rules will be evaluated at, which is also what
// turns the coverage policy in a detector into a sample count. It becomes
// each group's `interval`, so the file and the band agree about it rather
// than inheriting whatever the server's global default happens to be.
func Emit(target Target, step time.Duration, src Source, detectors ...spec.Detector) (Output, error) {
	var (
		out   Output
		file  ruleFile
		names = map[string]string{}
		notes []string
	)

	for _, d := range detectors {
		name, err := metricName(d.Metadata.Name)
		if err != nil {
			return Output{}, err
		}
		if other, taken := names[name]; taken {
			return Output{}, fmt.Errorf("%q and %q both compile to the series name %q: %w",
				other, d.Metadata.Name, name, ErrName)
		}
		names[name] = d.Metadata.Name

		if got := d.SourceType(); got != SourceType {
			return Output{}, fmt.Errorf("detector %q: %q: %w", d.Metadata.Name, got, ErrSourceType)
		}
		query, err := Query(d)
		if err != nil {
			return Output{}, err
		}

		band, err := d.Band(step)
		if err != nil {
			return Output{}, err
		}

		group, degraded, err := compile(target, name, d, band, query, step)
		if err != nil {
			return Output{}, err
		}
		file.Groups = append(file.Groups, group)
		if degraded != nil {
			out.Degradations = append(out.Degradations, *degraded)
			notes = append(notes, degraded.String())
		}
		notes = append(notes, fmt.Sprintf("%s: %s", d.Metadata.Name, describe(target, band)))
	}

	var body strings.Builder
	encoder := yaml.NewEncoder(&body)
	encoder.SetIndent(2)
	if err := encoder.Encode(file); err != nil {
		return Output{}, fmt.Errorf("encoding rules: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return Output{}, fmt.Errorf("encoding rules: %w", err)
	}
	out.Rules = append(header(step, src, notes), body.String()...)
	return out, nil
}

// VersionPrefix starts the first line of every rule file, before the version
// that wrote it.
const VersionPrefix = "# Generated by feno "

// IsVersionLine reports the header line naming the version that wrote the
// file.
//
// The drift gate compares every other line and not this one. What the gate
// answers is whether the committed rules still match the specifications: a
// specification that moved without being regenerated shows up as a changed
// hash, and a generated file somebody edited shows up in its body. An
// upgrade is neither of those. A release that changes the band arithmetic
// changes the expressions below, which are still compared byte for byte, so
// the release worth catching is caught by what it emitted rather than by the
// number on this line.
func IsVersionLine(line string) bool { return strings.HasPrefix(line, VersionPrefix) }

// header is the provenance spec.md Part 10 and Part 11 ask for: the tool that
// wrote the file, the specification behind it and a hash of that
// specification, plus which form of each band was chosen, so a file that
// changed shape between two runs explains itself in its own diff rather than
// in somebody's memory of how the server was started.
//
// The version is here so that a reader knows which release produced the
// expressions they are looking at, and a reader is the whole audience for it:
// [IsVersionLine] says why the gate does not read it.
func header(step time.Duration, src Source, notes []string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s. Do not edit.\n#\n", VersionPrefix, buildinfo.Get().Version)
	if src.Path != "" {
		fmt.Fprintf(&b, "# Source: %s\n", filepath.ToSlash(src.Path))
		fmt.Fprintf(&b, "# Specification hash: sha256:%x\n", sha256.Sum256(src.Specification))
	}
	fmt.Fprintf(&b, "# Evaluation interval: %s\n", promDuration(step))
	for _, note := range notes {
		fmt.Fprintf(&b, "# %s\n", note)
	}
	b.WriteString("#\n")
	return []byte(b.String())
}

// describe says which form of the band was emitted, in the terms the header
// prints.
func describe(target Target, band detect.Spec) string {
	parts := []string{string(band.Strategy)}
	if band.Strategy == detect.Robust {
		if useMAD(target, band) {
			parts = append(parts, "mad_over_time")
		} else {
			parts = append(parts, "interquartile range")
		}
	}
	parts = append(parts, fmt.Sprintf("window %s", promDuration(band.Window)))
	if len(band.Offsets) > 0 {
		parts = append(parts, fmt.Sprintf("%d pooled offsets", len(band.Offsets)))
	}
	return strings.Join(parts, ", ")
}

// useMAD reports whether this band compiles to `mad_over_time` on this
// target. A band that asked for it on a server without the feature falls
// back to the interquartile range, which is the same scale in the same
// units, and the fallback is declared as a [Degradation] rather than
// applied quietly.
func useMAD(target Target, band detect.Spec) bool {
	return band.RobustScale == detect.ScaleMAD && target.ExperimentalFunctions
}
