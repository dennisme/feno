package suggest

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dennisme/feno/internal/detect"
	"github.com/dennisme/feno/internal/rules"
	"github.com/dennisme/feno/internal/spec"
)

// commentWidth is the longest a header line may be, the comment marker and
// its indent included. Eighty is what the rest of this repository's YAML
// sits inside.
const commentWidth = 80

// The document a draft is, as typed structs in the order an author would
// have written them.
//
// These mirror a subset of [spec.AnomalyDetector] rather than marshalling it
// directly, and that costs one guard to be worth it. Marshalling the
// authoring type through sigs.k8s.io/yaml sorts the keys and writes out
// `window: ""` and an empty `alerting` for the fields a draft deliberately
// leaves absent, which is a document that claims choices nobody made. So the
// shape is written here, in reading order, with only what the draft sets.
//
// The guard is TestDraftLoads: every golden draft is loaded back through
// [spec.Load] and every decision is compared against what came out. A field
// renamed in internal/spec and not here fails there rather than in
// somebody's repository.
type document struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Metadata   named        `yaml:"metadata"`
	Spec       documentSpec `yaml:"spec"`
}

type named struct {
	Name string `yaml:"name"`
}

type documentSpec struct {
	Description string            `yaml:"description"`
	SLORef      string            `yaml:"sloRef,omitempty"`
	Indicator   documentIndicator `yaml:"indicator"`
	Detection   documentDetection `yaml:"detection"`
}

type documentIndicator struct {
	Metadata named                 `yaml:"metadata"`
	Spec     documentIndicatorSpec `yaml:"spec"`
}

type documentIndicatorSpec struct {
	ThresholdMetric documentMetric `yaml:"thresholdMetric"`
}

type documentMetric struct {
	MetricSource documentMetricSource `yaml:"metricSource"`
}

type documentMetricSource struct {
	Type            string                      `yaml:"type,omitempty"`
	MetricSourceRef string                      `yaml:"metricSourceRef,omitempty"`
	Spec            map[string]rules.Expression `yaml:"spec"`
}

type documentDetection struct {
	Strategy    detect.Strategy    `yaml:"strategy"`
	Sensitivity detect.Sensitivity `yaml:"sensitivity"`
	Seasonality []spec.Seasonality `yaml:"seasonality,omitempty"`
	Class       spec.Class         `yaml:"class"`
}

// Draft is the detector document, header and all.
//
// The version is in the header because a draft is reviewed and committed by
// a person, and the thresholds behind these decisions are judgements that
// will move between releases. A reviewer looking at a detector that fires
// too often a year from now needs to know which release's judgement it was.
func Draft(source Source, decision Decision, profile Profile, version string) ([]byte, error) {
	var body strings.Builder
	encoder := yaml.NewEncoder(&body)
	encoder.SetIndent(2)
	if err := encoder.Encode(documentOf(source, decision)); err != nil {
		return nil, fmt.Errorf("encoding the draft for SLI %q: %w", source.Name, err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encoding the draft for SLI %q: %w", source.Name, err)
	}
	return append(header(source, decision, profile, version), body.String()...), nil
}

func documentOf(source Source, decision Decision) document {
	return document{
		APIVersion: spec.APIVersion,
		Kind:       spec.Kind,
		Metadata:   named{Name: source.Name},
		Spec: documentSpec{
			Description: fmt.Sprintf("Drafted from SLI %s, %s.", source.Name, source.Origin),
			SLORef:      source.SLORef(),
			Indicator: documentIndicator{
				Metadata: named{Name: source.Name},
				Spec: documentIndicatorSpec{
					ThresholdMetric: documentMetric{
						MetricSource: documentMetricSource{
							Type:            source.Metric.Type,
							MetricSourceRef: source.Metric.MetricSourceRef,
							Spec:            map[string]rules.Expression{"query": rules.Expression(source.Query)},
						},
					},
				},
			},
			Detection: documentDetection{
				Strategy:    decision.Strategy,
				Sensitivity: decision.Sensitivity,
				Seasonality: decision.Seasonality,
				Class:       decision.Class,
			},
		},
	}
}

// header is what makes the draft reviewable rather than only acceptable: the
// evidence behind every decided field, what the profile read to get it, and
// which fields were left for a person.
func header(source Source, decision Decision, profile Profile, version string) []byte {
	var b strings.Builder
	write := func(format string, args ...any) {
		for _, line := range wrap(fmt.Sprintf(format, args...)) {
			b.WriteString(line)
		}
	}

	write("Drafted by feno %s from SLI %s in %s. Review every field before "+
		"committing it. Nothing here was written by a model.",
		version, source.Name, filepath.ToSlash(source.File))
	b.WriteString("#\n")
	write("Series banded: %s", source.Origin)
	write("Profiled: %d series over %s to %s at a %s step",
		len(profile.Series),
		profile.From.Format("2006-01-02T15:04:05Z"), profile.To.Format("2006-01-02T15:04:05Z"),
		duration(profile.Step))
	b.WriteString("#\n")
	for _, evidence := range decision.Evidence {
		write("%s", evidence)
	}
	b.WriteString("#\n")
	write("window, minCoverage and minWidth are absent, so feno's own defaults " +
		"apply. minWidth has no default and none is drafted: it is a floor in " +
		"this series' own units, which is a judgement about how large a change " +
		"is worth paging for rather than something a profile can measure.")
	b.WriteString("#\n")
	write("%s", sloNote(source))
	b.WriteString("#\n")
	return []byte(b.String())
}

// duration is a step as a header reads it, with the zero units Go's own
// formatting leaves on the end taken off: a five minute step is `5m` and not
// `5m0s`.
func duration(d time.Duration) string {
	text := d.String()
	for _, zero := range []string{"0s", "0m"} {
		if len(text) > len(zero) {
			text = strings.TrimSuffix(text, zero)
		}
	}
	return text
}

// sloNote says what the draft did about sloRef and why, which is the one
// field a person is most likely to have to finish.
func sloNote(source Source) string {
	switch len(source.SLOs) {
	case 0:
		return "sloRef: no SLO in this tree names this SLI, so the field is absent and a " +
			"backtest will report that nobody tied this detector to an error budget."
	case 1:
		return fmt.Sprintf("sloRef %s: the one SLO naming this SLI, so the link is unambiguous. "+
			"It is what lets a backtest say whether this detector's paging coincided with "+
			"that budget burning.", source.SLOs[0])
	default:
		return fmt.Sprintf("sloRef: %d SLOs name this SLI, %s. A detector is judged against one "+
			"error budget and nothing says which, so the field is absent. Set it to the "+
			"objective this detector protects.", len(source.SLOs), strings.Join(source.SLOs, ", "))
	}
}

// wrap breaks one note into comment lines at [commentWidth], indenting the
// continuations so that a long piece of evidence reads as one note rather
// than several.
//
// Wrapping is on words and never inside one, so a query or a statistic in
// the middle of a note stays greppable.
func wrap(text string) []string {
	var (
		lines  []string
		prefix = "# "
		line   string
	)
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(prefix)+len(line)+1+len(word) <= commentWidth:
			line += " " + word
		default:
			lines = append(lines, prefix+line+"\n")
			prefix = "#   "
			line = word
		}
	}
	if line != "" {
		lines = append(lines, prefix+line+"\n")
	}
	return lines
}
