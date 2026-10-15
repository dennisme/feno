package suggest

import (
	"errors"
	"fmt"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"

	"github.com/dennisme/feno/internal/rules"
	"github.com/dennisme/feno/internal/spec"
)

// ErrSLIShape reports an SLI no band can be drawn from. It is a refusal
// rather than a best effort, because every shape it covers would need a
// judgement about what the SLI means that only its author can make.
var ErrSLIShape = errors.New("no series in this SLI suits a band")

// Source is the SLI a draft is derived from, reduced to the one series the
// band will watch.
type Source struct {
	// Name and File are the SLI this came from, which the draft's header
	// names so that a reviewer can read the two documents side by side.
	Name string
	File string

	// Origin is which part of the SLI the query was taken from, written the
	// way the SLI's own YAML spells it: `thresholdMetric` or
	// `ratioMetric.total`.
	Origin string

	// Metric is the metric source as the SLI wrote it, carried through so
	// that the draft names the same connection. Copying it is what makes the
	// first detector cost no new configuration, which is the whole argument
	// for deriving one from an SLI.
	Metric v1.SLIMetricSource

	// Query is the PromQL that metric source holds.
	Query string

	// SLOs are the SLOs that named this SLI, in name order.
	SLOs []string
}

// NewSource is the series to band, from one SLI.
//
// # Which series a ratio SLI contributes
//
// A ratio SLI carries two or three queries and a band reads one series, so
// something has to choose. The choice is the ratio's `total`, the traffic,
// and the reason is what the detector is for: the burn alert already watches
// the ratio and compares it to the objective, faster and with the budget's
// own arithmetic behind it. A band over the same ratio is a second opinion
// about something already covered. What no ratio can see is its own
// denominator collapsing, where every remaining request succeeds and the
// objective is met by a service nobody can reach.
//
// So nothing here composes `good` and `total` into one query. A raw ratio is
// refused for the same reason it would be composed: it carries the ratio and
// no traffic series, so there is nothing to band that the burn alert does
// not already watch.
func NewSource(sli v1.SLI, file string, slos []string) (Source, error) {
	source := Source{Name: sli.Metadata.Name, File: file, SLOs: slos}

	switch {
	case sli.Spec.ThresholdMetric != nil:
		source.Origin = "thresholdMetric"
		source.Metric = sli.Spec.ThresholdMetric.MetricSource

	case sli.Spec.RatioMetric != nil:
		ratio := *sli.Spec.RatioMetric
		switch {
		case ratio.Raw != nil || ratio.RawType != "":
			return Source{}, fmt.Errorf("SLI %q: its ratio is raw, so it carries the ratio itself and no "+
				"traffic series, and the burn alert already watches the ratio: %w", sli.Metadata.Name, ErrSLIShape)
		case ratio.Total == nil:
			return Source{}, fmt.Errorf("SLI %q: its ratio declares no total, so there is no traffic series "+
				"to band: %w", sli.Metadata.Name, ErrSLIShape)
		}
		source.Origin = "ratioMetric.total"
		source.Metric = ratio.Total.MetricSource

	default:
		return Source{}, fmt.Errorf("SLI %q: it declares neither a thresholdMetric nor a ratioMetric: %w",
			sli.Metadata.Name, ErrSLIShape)
	}

	// A type written inline is the author saying where the query runs. An
	// SLI whose source is a reference says nothing here, because the loader
	// resolves a detector's references and not an SLI's, and the endpoint
	// the command was given is the only one a profile can read.
	if t := source.Metric.Type; t != "" && t != rules.SourceType {
		return Source{}, fmt.Errorf("SLI %q: %q: %w", sli.Metadata.Name, t, rules.ErrSourceType)
	}

	query, err := rules.SourceQuery(source.Metric)
	if err != nil {
		return Source{}, fmt.Errorf("SLI %q %s: %w", sli.Metadata.Name, source.Origin, err)
	}
	source.Query = query
	return source, nil
}

// SLORef is the SLO the draft names, which is the one SLO that named this
// SLI and nothing at all otherwise.
//
// Two SLOs over one SLI is the case a draft cannot resolve. A detector is
// judged against one error budget, the link is what makes the budget overlap
// in a backtest report computable, and guessing which of two objectives a
// detector protects would put a wrong answer somewhere nobody looks again.
// So the field is left out and the candidates go in the header for a person
// to choose from.
func (s Source) SLORef() string {
	if len(s.SLOs) == 1 {
		return s.SLOs[0]
	}
	return ""
}

// SLIDocument is one SLI and the file it was read from.
type SLIDocument struct {
	SLI  v1.SLI
	File string
}

// SLIs is every SLI a bundle holds, in the order the loader read them, which
// is file name order.
func SLIs(bundle spec.Bundle) []SLIDocument {
	var out []SLIDocument
	for _, object := range bundle.Objects {
		if sli, ok := object.Object.(v1.SLI); ok {
			out = append(out, SLIDocument{SLI: sli, File: object.File})
		}
	}
	return out
}

// SLOsNaming is every SLO in the bundle whose indicatorRef names the SLI, in
// the order the loader read them.
//
// Only an indicatorRef counts. An SLO that declares its indicator inline has
// no reference to this SLI and may happen to carry the same query, which is
// a coincidence rather than a link, and linking on it would make a draft's
// sloRef depend on two authors having written the same PromQL.
func SLOsNaming(bundle spec.Bundle, sli string) []string {
	var out []string
	for _, object := range bundle.Objects {
		slo, ok := object.Object.(v1.SLO)
		if !ok || slo.Spec.IndicatorRef == nil || *slo.Spec.IndicatorRef != sli {
			continue
		}
		out = append(out, slo.Metadata.Name)
	}
	return out
}
