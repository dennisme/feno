package rules

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"

	"github.com/dennisme/feno/internal/detect"
	"github.com/dennisme/feno/internal/prometheus"
	"github.com/dennisme/feno/internal/spec"
)

// The grid the comparison is made on. One minute is the interval a rule is
// evaluated at in every example in this repository, and the window is long
// enough that a quantile lands between two samples rather than on one, which
// is where two implementations of a quantile stop agreeing.
const (
	parityStep   = time.Minute
	parityWindow = 30 * time.Minute

	// parityFrom and parityTo are the instants the band is read at. They
	// start at twice the window, so the first one already has a full window
	// behind it, and they span a further quarter of an hour because one
	// instant cannot catch a window that is off by a sample: a band drawn
	// from 29 samples instead of 30 agrees at some instants and not others.
	parityFrom = 60 * time.Minute
	parityTo   = 75 * time.Minute

	// paritySeries is the metric the input series carries. It is a bare name
	// so that the compiled `level` rule records exactly what the test fed in.
	paritySeries = "band_input"
)

// TestEmittedBandMatchesEvaluatedBand is the agreement nothing else in this
// repository checks.
//
// The band is implemented twice: internal/detect computes it in Go for the
// backtest, and this package compiles it into PromQL for the server to
// compute. Every report feno prints is a claim about what the compiled rule
// will do, and until this test existed the only thing standing behind that
// claim was that both were written from the same spec.md section. A golden
// file pins the text we emit against text we wrote, and `promtool check
// rules` reads it for syntax. Neither has ever put a number from one next to
// a number from the other.
//
// So this feeds one series to both. promtool evaluates the real rule chain
// at the real interval, which is what makes the recorded intermediate series
// part of the test rather than something inlined away, and the expected
// values are whatever [detect.Evaluate] said over the same samples.
func TestEmittedBandMatchesEvaluatedBand(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("promtool not installed; `just init` installs the pinned version")
	}

	tests := []struct {
		name  string
		band  spec.Detection
		mad   bool
		input string
	}{
		{
			name: "adaptive",
			band: spec.Detection{
				Strategy:    detect.Adaptive,
				Sensitivity: detect.Balanced,
				Class:       spec.ClassRequests,
			},
			input: "10+1x75",
		},
		{
			name: "robust with an interquartile range",
			band: spec.Detection{
				Strategy:    detect.Robust,
				RobustScale: detect.ScaleIQR,
				Sensitivity: detect.Balanced,
				Class:       spec.ClassRequests,
			},
			input: "10+1x75",
		},
		{
			name: "robust with a median absolute deviation",
			band: spec.Detection{
				Strategy:    detect.Robust,
				RobustScale: detect.ScaleMAD,
				Sensitivity: detect.Balanced,
				Class:       spec.ClassRequests,
			},
			mad:   true,
			input: "10+1x75",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.mad {
				// promtool 3.15.0 does not enable experimental functions in
				// its unit tests, in either flag position, so the one band
				// that compiles to mad_over_time cannot be evaluated here
				// and this is the gap rather than a passing case:
				//
				//   rule: anomaly:parity:scale, err: parse error:
				//   function "mad_over_time" is not enabled
				//
				// What covers it instead is a server started with the
				// feature on, which needs seeded history as well as the
				// flag. Until that exists, the median absolute deviation's
				// arithmetic is unverified against anything but itself.
				t.Skip("promtool unit tests cannot evaluate mad_over_time; needs a flagged server with history")
			}

			detection := tt.band
			detection.Window = v1.NewDurationShorthand(30, v1.DurationShorthandUnitMinute)
			detector := parityDetector(detection)

			band, err := detector.Band(parityStep)
			if err != nil {
				t.Fatalf("Band: %v", err)
			}

			// The Go side, over the same samples promtool is about to be
			// given, read at the one instant the assertions are made at.
			values := ramp(tt.input)
			results, err := detect.Evaluate(band, detect.Range{
				Start: epoch(parityFrom),
				End:   epoch(parityTo),
				Step:  parityStep,
			}, points(values))
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			for _, result := range results {
				if result.Status != detect.Evaluated {
					t.Fatalf("status %q at %s, want a band: the input series is too short",
						result.Status, result.At.Sub(epoch(0)))
				}
			}

			out, err := Emit(Target{ExperimentalFunctions: tt.mad}, parityStep,
				Source{Path: "parity.yaml", Specification: []byte("parity")}, detector)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if len(out.Degradations) > 0 {
				t.Fatalf("the band compiled in a weaker form: %v", out.Degradations)
			}

			dir := t.TempDir()
			ruleFile := filepath.Join(dir, "rules.yml")
			if err := os.WriteFile(ruleFile, out.Rules, 0o600); err != nil {
				t.Fatalf("writing the rule file: %v", err)
			}
			unitFile := filepath.Join(dir, "unit.yml")
			if err := os.WriteFile(unitFile, unitTest("rules.yml", tt.input, results), 0o600); err != nil {
				t.Fatalf("writing the unit test: %v", err)
			}

			// The flag goes before the subcommand. promtool accepts it in
			// either position and only honours it here, which is the
			// difference between a band that evaluates and a parse error.
			var args []string
			if tt.mad {
				args = append(args, "--enable-feature=promql-experimental-functions")
			}
			args = append(args, "test", "rules", unitFile)
			output, err := exec.Command(promtool, args...).CombinedOutput()
			if err != nil {
				t.Errorf("the compiled rules disagree with internal/detect over %d instants: %v\n%s",
					len(results), err, output)
			}
		})
	}
}

// parityDetector is the smallest detector that compiles: one bare metric, one
// band, and a name that survives [metricName].
func parityDetector(detection spec.Detection) spec.Detector {
	return spec.Detector{
		AnomalyDetector: spec.AnomalyDetector{
			APIVersion: spec.APIVersion,
			Kind:       spec.Kind,
			Metadata:   v1.Metadata{Name: "parity"},
			Spec: spec.DetectorSpec{
				Indicator: v1.SLOIndicatorInline{
					Metadata: v1.Metadata{Name: "parity"},
					Spec: v1.SLISpec{
						ThresholdMetric: &v1.SLIMetricSpec{
							MetricSource: v1.SLIMetricSource{
								Type: SourceType,
								Spec: map[string]any{"query": paritySeries},
							},
						},
					},
				},
				Detection: detection,
			},
		},
		File: "parity.yaml",
	}
}

// unitTest is the promtool unit test: the series promtool feeds the rules,
// and the band internal/detect computed over that same series as the
// expectation, at every instant.
//
// Each edge is asserted rather than only the breach, because an upper and a
// lower edge that are both wrong by the same amount still compare correctly
// against the level they were derived from. The centre and the scale are
// asserted too, so a failure says which half of the band moved.
func unitTest(ruleFile, input string, results []detect.Result) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "rule_files:\n  - %s\n\n", ruleFile)
	fmt.Fprintf(&b, "evaluation_interval: %s\n\n", promDuration(parityStep))
	b.WriteString("tests:\n")
	fmt.Fprintf(&b, "  - interval: %s\n", promDuration(parityStep))
	b.WriteString("    input_series:\n")
	fmt.Fprintf(&b, "      - series: %s\n        values: '%s'\n", paritySeries, input)
	b.WriteString("    promql_expr_test:\n")
	for _, result := range results {
		at := result.At.Sub(epoch(0))
		for _, assertion := range []struct {
			expr  string
			value float64
		}{
			{expr: "anomaly:parity:centre", value: result.Band.Centre},
			{expr: "anomaly:parity:scale", value: result.Band.Scale},
			{expr: "anomaly:parity:upper_band", value: result.Band.Upper()},
			{expr: "anomaly:parity:lower_band", value: result.Band.Lower()},
		} {
			fmt.Fprintf(&b, "      - expr: %s\n", assertion.expr)
			fmt.Fprintf(&b, "        eval_time: %s\n", promDuration(at))
			b.WriteString("        exp_samples:\n")
			fmt.Fprintf(&b, "          - labels: '%s'\n", assertion.expr)
			fmt.Fprintf(&b, "            value: %s\n", strconv.FormatFloat(assertion.value, 'g', -1, 64))
		}
	}
	return []byte(b.String())
}

// ramp expands the one promtool series notation this test uses, `start+stepxN`,
// so that the samples promtool reads and the samples internal/detect reads
// come from one declaration rather than two that have to be kept in step.
func ramp(input string) []float64 {
	var start, step float64
	var count int
	if _, err := fmt.Sscanf(input, "%g+%gx%d", &start, &step, &count); err != nil {
		panic("unsupported series notation: " + input)
	}
	values := make([]float64, 0, count+1)
	for i := 0; i <= count; i++ {
		values = append(values, start+step*float64(i))
	}
	return values
}

// points puts the values on the same grid promtool evaluates on: one sample
// per step from the epoch, which is where a promtool unit test starts.
func points(values []float64) []prometheus.Point {
	out := make([]prometheus.Point, 0, len(values))
	for i, v := range values {
		out = append(out, prometheus.Point{At: epoch(time.Duration(i) * parityStep), Value: v})
	}
	return out
}

func epoch(d time.Duration) time.Time { return time.Unix(0, 0).UTC().Add(d) }
