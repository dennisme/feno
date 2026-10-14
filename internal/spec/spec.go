// Package spec holds the detector kind, the loader that reads a directory of
// mixed OpenSLO and detector documents, and the validation over ours.
//
// # Why the specification is OpenSLO's, mechanically
//
// A team that already keeps SLOs in git has declared the two expensive
// things: a DataSource says where the metrics live and how to connect, an
// SLI says which query matters. Writing a second way to say either of those
// would make the first useful command in this tool a migration. So the kind
// defined here is an OpenSLO-shaped object in its own API group, and the
// parts it has in common with an SLI are imported from
// github.com/OpenSLO/go-sdk rather than written again.
//
// Nothing here restates an OpenSLO type. [AnomalyDetector] carries
// v1.Metadata, v1.SLOIndicatorInline and v1.DurationShorthand by embedding
// and by field, and a resolved metricSourceRef is a v1.DataSourceSpec. A
// fork of those types is the failure this package exists to avoid: the
// moment `metricSourceRef` means something slightly different here than it
// does in an SLI next to it in the same directory, the claim that one
// directory describes both is false.
//
// # Where this disagrees with spec.md
//
// Part 2's example YAML puts `metricSource` directly under `indicator.spec`,
// alongside a null `ratioMetric`. OpenSLO does not have that shape: in
// OpenSLO an inline indicator's spec is an SLISpec, so a metric source sits
// under `thresholdMetric` or inside `ratioMetric`. Part 6 decision 2 says to
// reuse the indicator shape verbatim, and verbatim wins over the sketch, so
// a detector's indicator is a v1.SLOIndicatorInline and the query goes under
// `thresholdMetric`. The example in Part 2 needs the extra level of nesting.
//
// `ratioMetric` is rejected on a detector rather than supported. A ratio is
// two or three queries and internal/detect reads one series, so a band over
// a ratio has to be a band over a query that already computes the ratio.
// Composing good and total into one query is a judgement about what the
// ratio means, which belongs in `suggest` where a person reviews the draft,
// not in a loader that would do it silently.
//
// # Two dependencies, and what each one is for
//
// go-sdk supplies the OpenSLO types, their decoder and their validator.
// sigs.k8s.io/yaml is the YAML parser, and it is the right one rather than a
// preference: it converts YAML to JSON and decodes through encoding/json, so
// the `json` tags on the embedded go-sdk types are honoured. A parser that
// reads `yaml` tags would silently decode an embedded SLIMetricSource by
// lowercased field name and lose `metricSourceRef` on every document. It is
// also already in the build graph as go-sdk's own parser, so the two agreed
// dependencies cost one new module between them.
//
// Validation of our kind is written by hand rather than with govy, which
// go-sdk validates with. govy stays an indirect dependency, so `go mod
// graph` shows go-sdk owning it, and the errors here read like the ones in
// internal/detect and internal/prometheus: a sentinel wrapped with the file,
// the name and the value that was wrong.
package spec

import (
	"errors"
	"fmt"
	"slices"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"

	"github.com/dennisme/feno/internal/detect"
)

// APIVersion is the group and version detectors are declared in. It is a
// separate group from OpenSLO's on purpose: `oslo validate` must keep
// passing on the OpenSLO documents in the same directory, which means it has
// to be able to tell which documents are not its business.
const APIVersion = "anomaly.openslo.io/v1alpha"

// Kind is the only kind this group defines.
const Kind = "AnomalyDetector"

// Seasonality is a named period a band takes its history from. These names
// exist here and nowhere else: internal/detect holds offsets as durations
// and has no opinion about what a day is.
type Seasonality string

const (
	// Daily pools the same time of day on preceding days.
	Daily Seasonality = "daily"

	// Weekly pools the same time of week on preceding weeks, which is the
	// one that separates a Tuesday from a Sunday.
	Weekly Seasonality = "weekly"
)

// Class is what kind of signal the series carries.
//
// Nothing in [AnomalyDetector.Band] reads it, because every field of
// [detect.Spec] is a statistic or a duration and a class is neither. It is
// carried because it is the field that will decide whether a breach below
// the band counts: a drop in request rate is an incident and a drop in error
// rate is a Tuesday. One-sided detection is not in [detect.Spec] yet, and
// adding a direction to the band is a change to internal/detect rather than
// a thing to approximate here.
type Class string

const (
	ClassRequests   Class = "requests"
	ClassLatency    Class = "latency"
	ClassErrors     Class = "errors"
	ClassSaturation Class = "saturation"
)

var validClasses = []Class{ClassRequests, ClassLatency, ClassErrors, ClassSaturation}

// AnomalyDetector is one detector, as authored.
//
// APIVersion and Kind are plain strings rather than go-sdk's openslo.Version
// and openslo.Kind, which reject anything outside OpenSLO's own groups in
// UnmarshalText. Using them would make every detector fail to decode.
type AnomalyDetector struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   v1.Metadata  `json:"metadata"`
	Spec       DetectorSpec `json:"spec"`
}

// DetectorSpec is what the detector watches and how. It is named for its
// kind the way go-sdk names SLISpec and DataSourceSpec, which also keeps it
// distinct from [detect.Spec] in a file that reads both.
type DetectorSpec struct {
	Description string                `json:"description,omitempty"`
	Indicator   v1.SLOIndicatorInline `json:"indicator"`
	Detection   Detection             `json:"detection"`
	Alerting    Alerting              `json:"alerting,omitempty"`

	// SLORef names the SLO whose error budget this detector is judged
	// against, and is what makes the budget overlap in a backtest report
	// computable: an episode that never coincided with that budget burning
	// is a candidate for deletion.
	//
	// It is a field of its own rather than something inferred. A shared
	// metricSourceRef is the connection and many SLOs share one, so it says
	// nothing about which objective this detector protects. An indicator
	// name matching an SLI is a link that disappears when somebody renames
	// one, with nothing in either document saying a link was meant. Empty is
	// allowed and is a detector nobody has tied to an objective, which the
	// report says rather than reporting a zero overlap.
	SLORef string `json:"sloRef,omitempty"`
}

// Detection is the band, in the terms an author writes rather than the terms
// it is computed in. [AnomalyDetector.Band] is where the two meet.
//
// Strategy, Sensitivity and RobustScale are internal/detect's own types
// because their values are already the strings this YAML carries. Declaring
// a second set of constants here and a map between them would be two places
// to add `robust` to.
type Detection struct {
	Strategy    detect.Strategy    `json:"strategy"`
	RobustScale detect.RobustScale `json:"robustScale,omitempty"`
	Sensitivity detect.Sensitivity `json:"sensitivity"`
	Seasonality []Seasonality      `json:"seasonality,omitempty"`
	Class       Class              `json:"class"`

	// Window is how much history one band is computed over, and with
	// seasonality it is the width of the window taken at each offset rather
	// than a span reaching back to them. Empty means [DefaultWindow].
	Window v1.DurationShorthand `json:"window,omitempty"`

	// MinWidth is a floor on the band's full width in the series' own units.
	// Zero means no floor. There is no default and no value derived from
	// Class, because the units are the series' and nothing here knows them:
	// a floor of 1 is sensible for a count of errors and meaningless for a
	// ratio.
	MinWidth float64 `json:"minWidth,omitempty"`

	// MinCoverage is the fraction of a full window that must hold samples
	// before a band is computed. Zero means [DefaultMinCoverage]. See
	// [AnomalyDetector.Band] for why this is a fraction rather than the
	// sample count detect.Spec wants.
	MinCoverage float64 `json:"minCoverage,omitempty"`
}

// Alerting is what happens once the band is breached.
//
// Nothing here reaches [detect.Spec]. For is internal/episode's hold
// duration and the emitted rule's `for`, and Runbook is an annotation on the
// emitted rule. Both are carried so that one document describes the whole
// detector, and both are consumed by packages that do not exist yet.
type Alerting struct {
	For     v1.DurationShorthand `json:"for,omitempty"`
	Runbook string               `json:"runbook,omitempty"`
}

var (
	// ErrAPIVersion reports a detector carrying an apiVersion this group
	// does not define.
	ErrAPIVersion = errors.New("unsupported apiVersion")

	// ErrKind reports a kind this group does not define.
	ErrKind = errors.New("unsupported kind")

	// ErrName reports a missing metadata name. The name is what a generated
	// rule group is called and what an error elsewhere points at, so a
	// detector without one cannot be reported on.
	//
	// Only its presence is checked here. OpenSLO requires its own kinds'
	// names to be DNS labels and go-sdk enforces that on them, but what a
	// detector's name has to look like depends on what it is compiled into:
	// a Prometheus rule group and a ClickHouse view do not agree. That check
	// belongs to whichever emitter has the constraint.
	ErrName = errors.New("metadata.name is required")

	// ErrIndicator reports an indicator that does not describe exactly one
	// series.
	ErrIndicator = errors.New("indicator must carry one thresholdMetric")

	// ErrMetricSource reports a metric source that names neither a
	// datasource type nor a metricSourceRef, or names both.
	ErrMetricSource = errors.New("metric source must set exactly one of type or metricSourceRef")

	// ErrQuery reports a metric source with no query. The reference supplies
	// the connection and never the query, so an empty spec is empty whether
	// or not a metricSourceRef resolved.
	ErrQuery = errors.New("metric source spec is required")

	// ErrSeasonality reports an unknown or repeated seasonality name.
	ErrSeasonality = errors.New("unknown seasonality")

	// ErrClass reports an unknown class.
	ErrClass = errors.New("unknown class")

	// ErrWindow reports a window that is missing a unit, not positive, or
	// shorter than the step it would be sampled at.
	ErrWindow = errors.New("window")

	// ErrMinCoverage reports a coverage fraction outside (0, 1].
	ErrMinCoverage = errors.New("minimum coverage must be above 0 and at most 1")

	// ErrFor reports an alerting `for` that is not a valid duration.
	ErrFor = errors.New("alerting for")
)

// Validate reports whether the detector describes something that can be
// loaded and compiled. It does not check the band itself, because a band is
// only complete once a step is known: [AnomalyDetector.Band] runs
// [detect.Spec.Validate] over the result.
func (d AnomalyDetector) Validate() error {
	if d.APIVersion != APIVersion {
		return fmt.Errorf("%q: %w", d.APIVersion, ErrAPIVersion)
	}
	if d.Kind != Kind {
		return fmt.Errorf("%q: %w", d.Kind, ErrKind)
	}
	if d.Metadata.Name == "" {
		return ErrName
	}

	source, err := d.MetricSource()
	if err != nil {
		return err
	}
	switch {
	case (source.Type == "") == (source.MetricSourceRef == ""):
		return fmt.Errorf("type %q, metricSourceRef %q: %w",
			source.Type, source.MetricSourceRef, ErrMetricSource)
	case len(source.Spec) == 0:
		return ErrQuery
	}

	if !slices.Contains(validClasses, d.Spec.Detection.Class) {
		return fmt.Errorf("%q: %w", d.Spec.Detection.Class, ErrClass)
	}
	for i, s := range d.Spec.Detection.Seasonality {
		if _, ok := periods[s]; !ok {
			return fmt.Errorf("%q: %w", s, ErrSeasonality)
		}
		if slices.Contains(d.Spec.Detection.Seasonality[:i], s) {
			return fmt.Errorf("%q appears twice: %w", s, ErrSeasonality)
		}
	}
	if c := d.Spec.Detection.MinCoverage; c != 0 && (c < 0 || c > 1) {
		return fmt.Errorf("%v: %w", c, ErrMinCoverage)
	}
	// A DurationShorthand of zero stringifies to empty whatever unit it
	// carries, so `window: 0h` would otherwise read as absent and silently
	// take DefaultWindow. The unit is what says the author wrote something.
	window := d.Spec.Detection.Window
	if window.GetUnit() != "" && window.GetValue() <= 0 {
		return fmt.Errorf("%w %d%s: must be positive", ErrWindow, window.GetValue(), window.GetUnit())
	}
	if window.String() != "" {
		if err := window.Validate(); err != nil {
			return fmt.Errorf("%w %q: %w", ErrWindow, window.String(), err)
		}
	}
	if f := d.Spec.Alerting.For; f.String() != "" {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("%w %q: %w", ErrFor, f.String(), err)
		}
	}
	return nil
}

// MetricSource is the one metric source a detector watches.
//
// It is exported because each target reads a different thing out of it: the
// Spec map is OpenSLO's per-type extension point, so the key holding a
// PromQL query is not the key holding a ClickHouse one, and an accessor per
// target here would mean this package knowing every dialect.
func (d AnomalyDetector) MetricSource() (v1.SLIMetricSource, error) {
	sli := d.Spec.Indicator.Spec
	if sli.ThresholdMetric == nil || sli.RatioMetric != nil {
		return v1.SLIMetricSource{}, ErrIndicator
	}
	return sli.ThresholdMetric.MetricSource, nil
}
