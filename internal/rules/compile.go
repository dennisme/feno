package rules

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dennisme/feno/internal/detect"
	"github.com/dennisme/feno/internal/spec"
)

// The rule file, as typed structs rather than a template.
//
// Prometheus's own rule types live in github.com/prometheus/prometheus,
// which is a dependency the size of the server for six fields, so these are
// ours. They are unexported on purpose: what this package offers is a file
// that has been through the capability check, and an exported struct invites
// a caller to assemble a group by hand and skip it. A template would give
// the same bytes and no compiler help with a missing field, which is the
// whole argument for structs.
//
// These carry `yaml` tags and encode with go.yaml.in/yaml/v3 rather than
// through sigs.k8s.io/yaml, which internal/spec reads with. The reason is
// the reader on call at three in the morning. Encoding through
// encoding/json loses field order, so every rule prints `expr` above
// `record` and every group prints `interval` above `name`, and it folds a
// long expression across lines mid-expression. yaml/v3 keeps the order the
// struct declares and lets an expression pick its own scalar style. The
// module is already in the build graph as sigs.k8s.io/yaml's own parser, so
// this promotes an indirect dependency rather than adding one.
type ruleFile struct {
	Groups []group `yaml:"groups"`
}

type group struct {
	Name     string `yaml:"name"`
	Interval string `yaml:"interval,omitempty"`
	Rules    []rule `yaml:"rules"`
}

type rule struct {
	Record      string            `yaml:"record,omitempty"`
	Alert       string            `yaml:"alert,omitempty"`
	Expr        expression        `yaml:"expr"`
	For         string            `yaml:"for,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// expression is a PromQL expression that chooses how it is written out.
//
// A plain scalar is what a short expression wants, and the emitter wraps a
// long one across lines, which puts a line break in the middle of a metric
// name. It still parses, because a folded scalar rejoins on a space, but it
// is unreadable in exactly the file somebody is reading under pressure. So
// anything long or already multi-line becomes a literal block, which is
// what the Grafana reference library writes too.
type expression string

// foldWidth is where the encoder would start wrapping a plain scalar.
const foldWidth = 72

func (e expression) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.ScalarNode, Value: string(e)}
	if len(e) > foldWidth || strings.Contains(string(e), "\n") {
		node.Style = yaml.LiteralStyle
	}
	return node, nil
}

// AlertName is what every generated alerting rule is called.
//
// One name for every detector, distinguished by labels, which is how
// Prometheus routing is meant to work and what the Grafana reference library
// does. A name per detector would make a receiver have to enumerate them.
const AlertName = "AnomalyDetected"

// queryKey is where a Prometheus metric source keeps its PromQL, matching
// what OpenSLO's own SLI examples put there.
const queryKey = "query"

// metricNameChars is what a Prometheus metric name may contain after the
// first character.
var metricNameChars = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// compile turns one detector into its group.
func compile(
	target Target, name string, d spec.Detector, band detect.Spec, query string, step time.Duration,
) (group, *Degradation, error) {
	if band.Strategy == detect.Robust && len(band.Offsets) > 1 {
		return group{}, nil, fmt.Errorf(
			"detector %q: %s over %d pooled windows: a median of a pool is not a function of the per-window medians: %w",
			d.Metadata.Name, detect.RequireSeasonalDecomposition, len(band.Offsets), ErrUnsupported)
	}

	var (
		g        = group{Name: "anomaly-" + name, Interval: promDuration(step)}
		series   = func(suffix string) string { return "anomaly:" + name + ":" + suffix }
		level    = series("level")
		degraded *Degradation
	)

	g.Rules = append(g.Rules, rule{Record: level, Expr: expression(query)})

	centre, scale, pooled := statistics(target, series, level, band)
	g.Rules = append(g.Rules, pooled...)
	g.Rules = append(g.Rules,
		rule{Record: series("centre"), Expr: expression(centre)},
		rule{Record: series("scale"), Expr: expression(scale)},
	)

	if band.RobustScale == detect.ScaleMAD && !target.ExperimentalFunctions {
		degraded = &Degradation{
			Detector:    d.Metadata.Name,
			Requirement: detect.RequireMAD,
			Substitute:  "interquartile range",
		}
	}

	halfWidth := fmt.Sprintf("%s * %s", number(band.Margin()), series("scale"))
	if band.MinWidth > 0 {
		halfWidth = fmt.Sprintf("clamp_min(%s, %s)", halfWidth, number(band.MinWidth/2))
	}
	g.Rules = append(g.Rules,
		rule{Record: series("half_width"), Expr: expression(halfWidth)},
		rule{Record: series("upper_band"), Expr: expression(series("centre") + " + " + series("half_width"))},
		rule{Record: series("lower_band"), Expr: expression(series("centre") + " - " + series("half_width"))},
	)

	g.Rules = append(g.Rules, alert(d, level, series))
	return g, degraded, nil
}

// statistics returns the centre and scale expressions, plus any recording
// rules they reference.
//
// One window compiles to the obvious thing. More than one needs the pooled
// mean and variance written out, which is why each window's mean and
// variance become recorded series: the scale expression reads every mean
// twice, once for the pooled mean and once for its deviation from it, and
// recomputing a window pass per reference costs more than a stored series.
func statistics(
	target Target, series func(string) string, level string, band detect.Spec,
) (centre, scale string, pooled []rule) {
	windows := band.Offsets
	if len(windows) == 0 {
		windows = []time.Duration{0}
	}

	if len(windows) == 1 {
		sel := selector(level, band.Window, windows[0])
		switch band.Strategy {
		case detect.Robust:
			return fmt.Sprintf("quantile_over_time(0.5, %s)", sel), robustScale(target, band, sel), nil
		default:
			return fmt.Sprintf("avg_over_time(%s)", sel), fmt.Sprintf("stddev_over_time(%s)", sel), nil
		}
	}

	means := make([]string, 0, len(windows))
	variances := make([]string, 0, len(windows))
	for _, offset := range windows {
		sel := selector(level, band.Window, offset)
		tag := promDuration(offset)
		mean, variance := series("mean_"+tag), series("var_"+tag)
		pooled = append(pooled,
			rule{Record: mean, Expr: expression(fmt.Sprintf("avg_over_time(%s)", sel))},
			rule{Record: variance, Expr: expression(fmt.Sprintf("stddev_over_time(%s) ^ 2", sel))},
		)
		means = append(means, mean)
		variances = append(variances, variance)
	}

	n := number(float64(len(windows)))
	centre = fmt.Sprintf("(%s) / %s", strings.Join(means, " + "), n)

	// The pooled population variance of equal-length windows is the mean of
	// their variances plus the variance of their means, which is what makes
	// this the same number internal/detect computes over the pool rather
	// than an approximation of it.
	spread := make([]string, 0, len(means))
	for _, mean := range means {
		spread = append(spread, fmt.Sprintf("(%s - %s) ^ 2", mean, series("centre")))
	}
	scale = fmt.Sprintf("sqrt(((%s) / %s) + ((%s) / %s))",
		strings.Join(variances, " + "), n, strings.Join(spread, " + "), n)
	return centre, scale, pooled
}

// robustScale is the median absolute deviation where the server has it and
// the interquartile range where it does not. Both are converted with
// internal/detect's own constants, so the sensitivity knob means the same
// width either way.
func robustScale(target Target, band detect.Spec, sel string) string {
	if useMAD(target, band) {
		return fmt.Sprintf("%s * mad_over_time(%s)", number(detect.MADToSigma), sel)
	}
	return fmt.Sprintf("(quantile_over_time(0.75, %s) - quantile_over_time(0.25, %s)) / %s",
		sel, sel, number(detect.IQRToSigma))
}

// alert is the rule that pages.
//
// The comparison is strict at both edges, matching [detect.Band.Breaches],
// and a NaN band makes both sides false so the series simply drops out,
// which is what internal/detect calls Undefined and a rule cannot tell from
// an absent sample.
//
// Only `anomaly_detector` is attached. A detector's OpenSLO labels are
// multi-valued and Prometheus labels are not, so copying them would mean
// inventing a character to join them with, and a routing tree built on an
// invented separator is worse than one built on the detector name.
func alert(d spec.Detector, level string, series func(string) string) rule {
	r := rule{
		Alert: AlertName,
		Expr: expression(fmt.Sprintf("%s > %s or %s < %s",
			level, series("upper_band"), level, series("lower_band"))),
		Labels: map[string]string{"anomaly_detector": d.Metadata.Name},
	}
	if f := d.Spec.Alerting.For; f.String() != "" {
		r.For = f.String()
	}
	annotations := map[string]string{}
	if d.Spec.Description != "" {
		annotations["description"] = d.Spec.Description
	}
	if d.Spec.Alerting.Runbook != "" {
		annotations["runbook_url"] = d.Spec.Alerting.Runbook
	}
	if len(annotations) > 0 {
		r.Annotations = annotations
	}
	return r
}

// selector is the range selector one window compiles to.
//
// The range is exactly Spec.Window. A Prometheus range selector is
// left-open and right-closed and internal/detect's window is the same, so
// the two cover the same samples and the backtest keeps predicting
// production at the instants either side of a step change.
func selector(level string, window, offset time.Duration) string {
	sel := fmt.Sprintf("%s[%s]", level, promDuration(window))
	if offset > 0 {
		sel += " offset " + promDuration(offset)
	}
	return sel
}

// Query is the detector's query, for this target.
//
// It is exported because internal/backtest reads the same key: the backtest
// evaluates the history behind the query the generated rule records, and two
// packages reading two different keys out of the same metric source is a
// backtest judging a series production never sees.
func Query(d spec.Detector) (string, error) {
	source, err := d.MetricSource()
	if err != nil {
		return "", fmt.Errorf("detector %q: %w", d.Metadata.Name, err)
	}
	query, ok := source.Spec[queryKey].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("detector %q: %w", d.Metadata.Name, ErrQuery)
	}
	return strings.TrimSpace(query), nil
}

// metricName is the detector's name as it appears inside a recorded series
// name.
//
// OpenSLO names are DNS labels, so hyphens are idiomatic, and a Prometheus
// metric name has no hyphens. Rewriting them to underscores keeps ordinary
// names working, and it can map two distinct detectors onto one series, so
// [Emit] checks the whole set for that rather than trusting the rewrite.
// Anything else that does not survive is refused instead of mangled: a
// generated series name is what somebody's dashboard will point at.
func metricName(name string) (string, error) {
	rewritten := strings.ReplaceAll(name, "-", "_")
	if !metricNameChars.MatchString(rewritten) {
		return "", fmt.Errorf("%q is not usable in a metric name: %w", name, ErrName)
	}
	return rewritten, nil
}

// number formats a float the way the rule file should carry it: exactly, and
// without an exponent where one is avoidable.
func number(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// promDuration formats a duration the way Prometheus parses one: components
// in descending order, each at most once, zero components dropped.
// time.Duration's own String would emit "24h0m0s", which parses but reads
// like a bug in the generator.
func promDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	var b strings.Builder
	for _, unit := range []struct {
		size   time.Duration
		suffix string
	}{
		{24 * time.Hour, "d"},
		{time.Hour, "h"},
		{time.Minute, "m"},
		{time.Second, "s"},
	} {
		if n := d / unit.size; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, unit.suffix)
			d -= n * unit.size
		}
	}
	if d > 0 {
		fmt.Fprintf(&b, "%dms", d/time.Millisecond)
	}
	return b.String()
}
