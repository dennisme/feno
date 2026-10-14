package rules

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"

	"github.com/dennisme/feno/internal/spec"
)

var (
	// ErrUnlinked reports a detector that names no SLO. It is the caller's
	// job to tell that apart from a broken link, because the report prints a
	// different sentence for each, so this is a misuse rather than a
	// specification error.
	ErrUnlinked = errors.New("detector names no SLO")

	// ErrObjectives reports an SLO with no objective, or with more than one.
	// Two objectives are two budgets burning at two rates, and picking one
	// would make the overlap number depend on the order they were written
	// in.
	ErrObjectives = errors.New("burn signal needs exactly one objective")

	// ErrComposite reports an objective carrying its own indicator. A
	// composite SLO's budget is a weighted sum over other objectives, which
	// is arithmetic over somebody else's SLOs rather than a query over one
	// indicator.
	ErrComposite = errors.New("composite objectives have no single burn signal")

	// ErrRatio reports a ratio indicator this cannot divide: no total, or
	// neither good nor bad, or both of them, or one given as a raw metric.
	ErrRatio = errors.New("ratio indicator must carry a total and exactly one of good or bad")

	// ErrIndicator reports an SLO indicator carrying neither a
	// thresholdMetric nor a ratioMetric.
	ErrIndicator = errors.New("indicator must carry a thresholdMetric or a ratioMetric")

	// ErrTarget reports a ratio objective whose target is not a fraction
	// below one. A target of one leaves no error budget, so there is no rate
	// at which it can be spent and nothing to compare an episode against.
	ErrTarget = errors.New("objective target must be above 0 and below 1")

	// ErrObjectiveValue reports a threshold objective missing the operator
	// or the value that says what good means.
	ErrObjectiveValue = errors.New("threshold objective must carry both op and value")

	// ErrSourceMismatch reports an SLO measured on a different datasource
	// from the detector linked to it. The burn signal is read through the
	// same client over the same window, so two sources would intersect a
	// firing timeline from one server with a budget from another.
	ErrSourceMismatch = errors.New("the linked SLO is measured on another datasource")
)

// Burn is the burn signal for one detector's linked SLO: the PromQL to read,
// and the comparison that says the error budget was being spent.
//
// The comparison is a field rather than part of the query. A query ending in
// `> bool 1` would return 0 and 1 for every instant, and a divide by zero in
// the SLI would come back as a 0, which reads as a budget that was not
// burning. Reading the value and comparing here keeps an instant the server
// could not compute distinguishable from one it computed as quiet, which is
// the same distinction internal/detect draws with [detect.Undefined].
type Burn struct {
	// SLO is the name the detector's sloRef resolved to, and Objective is
	// that objective in the terms it was written in, both so that a report
	// says which budget it intersected.
	SLO       string
	Objective string

	// Query is the PromQL read over the reported window, at the same step
	// the detector is evaluated at.
	Query string

	// Operator and Threshold are the comparison: the budget was burning at
	// an instant whose value satisfies them.
	Operator  v1.Operator
	Threshold float64
}

// At is whether the budget was burning at an instant worth this value, and
// whether the value says anything at all.
//
// A NaN or an infinity is not a quiet budget: it is an SLI that divided by
// zero or a counter that reset, and counting it as not burning would make
// every detector look less useful than it is.
func (b Burn) At(value float64) (burning, known bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return false, false
	}
	switch b.Operator {
	case v1.OperatorGT:
		return value > b.Threshold, true
	case v1.OperatorGTE:
		return value >= b.Threshold, true
	case v1.OperatorLT:
		return value < b.Threshold, true
	case v1.OperatorLTE:
		return value <= b.Threshold, true
	default:
		return false, false
	}
}

// Comparison is the test the query's value is put to, as a reader of a report
// would write it. The symbols are PromQL's, so the whole line can be pasted
// into a query window and it says what it meant.
func (b Burn) Comparison() string {
	symbols := map[v1.Operator]string{
		v1.OperatorGT:  ">",
		v1.OperatorGTE: ">=",
		v1.OperatorLT:  "<",
		v1.OperatorLTE: "<=",
	}
	symbol, ok := symbols[b.Operator]
	if !ok {
		symbol = string(b.Operator)
	}
	return fmt.Sprintf("%s %s", symbol, number(b.Threshold))
}

// BurnQuery compiles the burn signal for the SLO a detector links to.
//
// Two shapes of objective reach here and each says "the budget is being
// spent" differently:
//
//   - A ratio indicator with an occurrence target. The error ratio divided by
//     the allowance the target leaves is the burn rate, and above one the
//     budget is being spent faster than the objective permits. This is the
//     same quantity sloth and pyrra record, computed from the SLO rather
//     than read out of their output, so it needs neither their naming nor
//     their rules to be present.
//   - A threshold indicator with an op and a value. There is no rate to
//     compute: the budget is being spent at exactly the instants the
//     threshold is violated, so the comparison is the objective's own,
//     negated. The objective's `target`, the fraction of time the threshold
//     must hold, says how much may be spent over a window and not when, so
//     it has no bearing on which instant was burning.
//
// The multiwindow burn-rate alerts sloth and pyrra generate are a different
// thing again: a judgement about when to page, with short and long windows
// and a factor per severity. This is the budget burning, which is what an
// anomaly episode either did or did not coincide with.
func BurnQuery(d spec.Detector) (Burn, error) {
	if d.SLO == nil {
		return Burn{}, fmt.Errorf("detector %q: %w", d.Metadata.Name, ErrUnlinked)
	}

	where := fmt.Sprintf("detector %q linked to SLO %q", d.Metadata.Name, d.SLO.Metadata.Name)

	if len(d.SLO.Spec.Objectives) != 1 {
		return Burn{}, fmt.Errorf("%s: %d objectives: %w",
			where, len(d.SLO.Spec.Objectives), ErrObjectives)
	}
	objective := d.SLO.Spec.Objectives[0]
	if objective.Indicator != nil || objective.IndicatorRef != nil {
		return Burn{}, fmt.Errorf("%s: %w", where, ErrComposite)
	}

	if err := sameSource(d, d.SLO.Indicator); err != nil {
		return Burn{}, fmt.Errorf("%s: %w", where, err)
	}

	burn := Burn{SLO: d.SLO.Metadata.Name}
	indicator := d.SLO.Indicator

	switch {
	case indicator.RatioMetric != nil:
		errorRatio, err := errorRatio(*indicator.RatioMetric)
		if err != nil {
			return Burn{}, fmt.Errorf("%s: %w", where, err)
		}
		target, err := ratioTarget(objective)
		if err != nil {
			return Burn{}, fmt.Errorf("%s: %w", where, err)
		}
		burn.Query = fmt.Sprintf("%s / (1 - %s)", errorRatio, number(target))
		burn.Operator, burn.Threshold = v1.OperatorGT, 1
		burn.Objective = percentage(target)

	case indicator.ThresholdMetric != nil:
		if objective.Operator == "" || objective.Value == nil {
			return Burn{}, fmt.Errorf("%s: op %q: %w", where, objective.Operator, ErrObjectiveValue)
		}
		operator, err := negate(objective.Operator)
		if err != nil {
			return Burn{}, fmt.Errorf("%s: %w", where, err)
		}
		query, err := promQL(indicator.ThresholdMetric.MetricSource)
		if err != nil {
			return Burn{}, fmt.Errorf("%s: %w", where, err)
		}
		burn.Query = query
		burn.Operator, burn.Threshold = operator, *objective.Value
		burn.Objective = fmt.Sprintf("%s %s", objective.Operator, number(*objective.Value))

	default:
		return Burn{}, fmt.Errorf("%s: %w", where, ErrIndicator)
	}

	return burn, nil
}

// errorRatio is the fraction of the indicator that was bad, as PromQL.
//
// Every query is parenthesised before it is divided, because an SLI query is
// whatever its author wrote and PromQL's binary operators bind tighter than
// the reader of a specification expects: `a or b / c` is not `(a or b) / c`.
func errorRatio(metric v1.SLIRatioMetric) (string, error) {
	switch {
	case metric.Total == nil:
		return "", ErrRatio
	case (metric.Good == nil) == (metric.Bad == nil):
		return "", ErrRatio
	case metric.Raw != nil || metric.RawType != "":
		return "", ErrRatio
	}

	total, err := promQL(metric.Total.MetricSource)
	if err != nil {
		return "", err
	}
	if metric.Bad != nil {
		bad, err := promQL(metric.Bad.MetricSource)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("((%s) / (%s))", bad, total), nil
	}
	good, err := promQL(metric.Good.MetricSource)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("(1 - ((%s) / (%s)))", good, total), nil
}

// ratioTarget is the objective's target as a fraction, written either way
// OpenSLO allows.
func ratioTarget(objective v1.SLOObjective) (float64, error) {
	var target float64
	switch {
	case objective.Target != nil:
		target = *objective.Target
	case objective.TargetPercent != nil:
		// Rounded, because dividing 99.9 by 100 in binary floating point is
		// 0.9990000000000001 and the generated query would carry that tail
		// where the author wrote two decimal places.
		target = math.Round(*objective.TargetPercent*1e7) / 1e9
	default:
		return 0, ErrTarget
	}
	if target <= 0 || target >= 1 {
		return 0, fmt.Errorf("%v: %w", target, ErrTarget)
	}
	return target, nil
}

// negate turns "what good looks like" into "when the budget is burning".
func negate(operator v1.Operator) (v1.Operator, error) {
	switch operator {
	case v1.OperatorLT:
		return v1.OperatorGTE, nil
	case v1.OperatorLTE:
		return v1.OperatorGT, nil
	case v1.OperatorGT:
		return v1.OperatorLTE, nil
	case v1.OperatorGTE:
		return v1.OperatorLT, nil
	default:
		return "", fmt.Errorf("op %q: %w", operator, ErrObjectiveValue)
	}
}

// sameSource refuses an SLO measured somewhere other than where the detector
// is.
//
// A metric source names its datasource by type inline or by reference, and a
// reference on an SLI is not resolved by the loader, which only follows a
// detector's own. So what can be checked is what is written: a type that is
// not this target's, and a reference that is not the one the detector uses. A
// source declaring neither is read through the detector's client, which is
// the only client a backtest has.
func sameSource(d spec.Detector, indicator v1.SLISpec) error {
	own, err := d.MetricSource()
	if err != nil {
		return err
	}
	for _, source := range sources(indicator) {
		if source.Type != "" && source.Type != SourceType {
			return fmt.Errorf("%q: %w", source.Type, ErrSourceType)
		}
		if ref := source.MetricSourceRef; ref != "" && own.MetricSourceRef != "" && ref != own.MetricSourceRef {
			return fmt.Errorf("metricSourceRef %q against the detector's %q: %w",
				ref, own.MetricSourceRef, ErrSourceMismatch)
		}
	}
	return nil
}

// sources is every metric source an indicator reads, whichever shape it is.
func sources(indicator v1.SLISpec) []v1.SLIMetricSource {
	var out []v1.SLIMetricSource
	if indicator.ThresholdMetric != nil {
		out = append(out, indicator.ThresholdMetric.MetricSource)
	}
	if ratio := indicator.RatioMetric; ratio != nil {
		for _, metric := range []*v1.SLIMetricSpec{ratio.Good, ratio.Bad, ratio.Total, ratio.Raw} {
			if metric != nil {
				out = append(out, metric.MetricSource)
			}
		}
	}
	return out
}

// promQL is the query a metric source carries under the key this target
// reads, which is the same key [Query] reads a detector's out of.
func promQL(source v1.SLIMetricSource) (string, error) {
	query, ok := source.Spec[queryKey].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return "", ErrQuery
	}
	return strings.TrimSpace(query), nil
}

// percentage is a target as a report prints it. The rounding is there because
// a target is written in decimal and 0.999 times 100 is not exactly 99.9 in
// binary floating point, which would otherwise print a tail of nines.
func percentage(target float64) string {
	return strconv.FormatFloat(math.Round(target*1e9)/1e7, 'f', -1, 64) + "%"
}
