package spec

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"
)

// SLO is the objective a detector was linked to, with its indicator resolved.
//
// An OpenSLO SLO carries its indicator inline or names an SLI declared
// elsewhere, and which of the two an author chose is a property of their
// repository rather than of the objective. Resolving it here means the
// backtest reads one shape, and it has to be here because a reference across
// documents can only be followed by whatever holds every document.
type SLO struct {
	v1.SLO

	// Indicator is what the objective is measured over, whether the SLO
	// declared it inline or named it.
	Indicator v1.SLISpec
}

var (
	// ErrUnresolvedSLORef reports a sloRef naming an SLO that no loaded
	// document declares. It is an error rather than an unlinked detector,
	// because the report prints a different sentence for a detector that
	// names no SLO than for one whose name was wrong, and silently merging
	// the two would hide a typo as a finding about the detector.
	ErrUnresolvedSLORef = errors.New("spec.sloRef names no SLO")

	// ErrSLORefKind reports a sloRef naming a document that exists and is
	// not an SLO. An SLI is the near miss worth telling apart: the two often
	// share a name, and the burn signal needs the objective that the SLI
	// alone does not carry.
	ErrSLORefKind = errors.New("spec.sloRef does not name an SLO")

	// ErrUnresolvedIndicatorRef reports a linked SLO whose indicatorRef
	// names no SLI in the tree.
	ErrUnresolvedIndicatorRef = errors.New("the linked SLO's spec.indicatorRef names no SLI")

	// ErrIndicatorRefKind reports a linked SLO whose indicatorRef names a
	// document that is not an SLI.
	ErrIndicatorRefKind = errors.New("the linked SLO's spec.indicatorRef does not name an SLI")

	// ErrSLOIndicator reports a linked SLO that declares neither an indicator
	// nor an indicatorRef, so there is nothing to read a burn signal from.
	ErrSLOIndicator = errors.New("the linked SLO declares neither indicator nor indicatorRef")
)

// link resolves every detector's sloRef against the objects the same load
// collected, and resolves each linked SLO's indicator.
//
// Only the SLOs a detector actually named are resolved. An SLO nobody linked
// to is still kept in [Bundle.Objects] as the bytes its author wrote, and
// refusing a tree because an unlinked SLO points at a missing SLI would make
// this tool reject a repository over a document it has no business reading.
func link(detectors []Detector, slos map[string]*v1.SLO, slis map[string]*v1.SLI, seen map[string]string) error {
	for i := range detectors {
		ref := detectors[i].Spec.SLORef
		if ref == "" {
			continue
		}

		slo, ok := slos[ref]
		if !ok {
			if kind, where, found := declared(seen, ref); found {
				return fmt.Errorf("%s: detector %q sets sloRef %q, which is a %s declared in %s: %w",
					detectors[i].File, detectors[i].Metadata.Name, ref, kind, where, ErrSLORefKind)
			}
			return fmt.Errorf("%s: detector %q sets sloRef %q: %w",
				detectors[i].File, detectors[i].Metadata.Name, ref, ErrUnresolvedSLORef)
		}

		indicator, err := indicatorOf(*slo, slis, seen)
		if err != nil {
			return fmt.Errorf("%s: detector %q sets sloRef %q: %w",
				detectors[i].File, detectors[i].Metadata.Name, ref, err)
		}
		detectors[i].SLO = &SLO{SLO: *slo, Indicator: indicator}
	}
	return nil
}

// indicatorOf is the SLO's indicator, followed through an indicatorRef when
// that is how it was written.
func indicatorOf(slo v1.SLO, slis map[string]*v1.SLI, seen map[string]string) (v1.SLISpec, error) {
	switch {
	case slo.Spec.Indicator != nil:
		return slo.Spec.Indicator.Spec, nil
	case slo.Spec.IndicatorRef != nil:
		ref := *slo.Spec.IndicatorRef
		sli, ok := slis[ref]
		if ok {
			return sli.Spec, nil
		}
		if kind, where, found := declared(seen, ref); found {
			return v1.SLISpec{}, fmt.Errorf("SLO %q sets indicatorRef %q, which is a %s declared in %s: %w",
				slo.Metadata.Name, ref, kind, where, ErrIndicatorRefKind)
		}
		return v1.SLISpec{}, fmt.Errorf("SLO %q sets indicatorRef %q: %w",
			slo.Metadata.Name, ref, ErrUnresolvedIndicatorRef)
	default:
		return v1.SLISpec{}, fmt.Errorf("SLO %q: %w", slo.Metadata.Name, ErrSLOIndicator)
	}
}

// declared is the kind and the document that claimed a name, which is what
// tells a reference to the wrong kind apart from a reference to nothing. The
// keys are walked in order because a map would otherwise name whichever
// match it reached first.
func declared(seen map[string]string, name string) (kind, where string, found bool) {
	for _, key := range slices.Sorted(maps.Keys(seen)) {
		gotKind, gotName, _ := strings.Cut(key, "/")
		if gotName == name {
			return gotKind, seen[key], true
		}
	}
	return "", "", false
}
