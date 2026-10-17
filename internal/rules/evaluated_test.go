package rules

import (
	"errors"
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

// paritySeries is the metric the input series carries. It is a bare name so
// that the compiled `level` rule records exactly what the test fed in.
const paritySeries = "band_input"

// TestEmittedBandMatchesEvaluatedBand is the agreement nothing else in this
// repository checks.
//
// The band is implemented twice: internal/detect computes it in Go for the
// backtest, and this package compiles it into PromQL for the server to
// compute. Every report feno prints is a claim about what the compiled rule
// will do, and the only thing standing behind that claim is that both were
// written from the same section of spec.md. A golden file pins the text we
// emit against text we wrote, and `promtool check rules` reads it for
// syntax. Neither puts a number from one next to a number from the other.
//
// So this feeds one series to both. promtool evaluates the real rule chain
// at the real interval, which is what makes the recorded intermediate series
// part of the test rather than something inlined away, and the expected
// values are whatever [detect.Evaluate] said over the same samples.
//
// # Why the seasonal cases are the ones that matter
//
// A flat band is the same arithmetic twice. A seasonal band is not.
// internal/detect collects the raw samples from every offset window into one
// set and takes the mean and the standard deviation over all of it. PromQL
// cannot pool, so [statistics] records a mean and a variance per offset and
// combines them: the mean of the means, and the square root of the mean of
// the variances plus the variance of the means. The two are equal for
// equal-length windows, which is a claim about statistics rather than about
// code, and it is the one place here where a disagreement would be a
// different formula rather than a typo.
func TestEmittedBandMatchesEvaluatedBand(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("promtool not installed; `just init` installs the pinned version")
	}

	tests := []struct {
		name string
		band spec.Detection
		mad  bool

		// step is the interval the rules are evaluated at, window is how
		// much history one band is computed over, and span is how much of
		// the series past the band's lookback is asserted.
		//
		// The seasonal cases run at a coarser step with a wider window, and
		// both halves of that matter. promtool evaluates every rule from the
		// epoch to the last instant asserted, so a weekly band reaching back
		// three weeks costs three thousand evaluations at ten minutes and
		// thirty thousand at one. Widening the window with the step keeps six
		// samples in it, which is what the comparison needs.
		step   time.Duration
		window time.Duration
		span   time.Duration
	}{
		{
			name: "adaptive",
			band: spec.Detection{
				Strategy:    detect.Adaptive,
				Sensitivity: detect.Balanced,
				Class:       spec.ClassRequests,
			},
			step:   time.Minute,
			window: 30 * time.Minute,
			span:   15 * time.Minute,
		},
		{
			name: "robust with an interquartile range",
			band: spec.Detection{
				Strategy:    detect.Robust,
				RobustScale: detect.ScaleIQR,
				Sensitivity: detect.Balanced,
				Class:       spec.ClassRequests,
			},
			step:   time.Minute,
			window: 30 * time.Minute,
			span:   15 * time.Minute,
		},
		{
			name: "robust with a median absolute deviation",
			band: spec.Detection{
				Strategy:    detect.Robust,
				RobustScale: detect.ScaleMAD,
				Sensitivity: detect.Balanced,
				Class:       spec.ClassRequests,
			},
			mad:    true,
			step:   time.Minute,
			window: 30 * time.Minute,
			span:   15 * time.Minute,
		},
		{
			name: "adaptive pooled over three days",
			band: spec.Detection{
				Strategy:    detect.Adaptive,
				Sensitivity: detect.Balanced,
				Class:       spec.ClassRequests,
				Seasonality: []spec.Seasonality{spec.Daily},
			},
			step:   10 * time.Minute,
			window: time.Hour,
			span:   time.Hour,
		},
		{
			name: "adaptive pooled over three weeks",
			band: spec.Detection{
				Strategy:    detect.Adaptive,
				Sensitivity: detect.Balanced,
				Class:       spec.ClassRequests,
				Seasonality: []spec.Seasonality{spec.Weekly},
			},
			step:   10 * time.Minute,
			window: time.Hour,
			span:   time.Hour,
		},
		{
			name: "adaptive pooled over both periods",
			band: spec.Detection{
				Strategy:    detect.Adaptive,
				Sensitivity: detect.Balanced,
				Class:       spec.ClassRequests,
				Seasonality: []spec.Seasonality{spec.Daily, spec.Weekly},
			},
			step:   10 * time.Minute,
			window: time.Hour,
			span:   time.Hour,
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
				// The flag reaches the rule manager and loads the file. The
				// engine the unit test evaluates with builds its own parser
				// and takes the default options, so the expression is
				// rejected the second time it is parsed.
				//
				// https://github.com/prometheus/prometheus/pull/20046 passes
				// the configured parser through. Once that is released and
				// .tool-versions moves to it, delete this branch: the case
				// is already in the table above and needs nothing else to
				// run. Until then the median absolute deviation's arithmetic
				// is unverified against anything but itself.
				t.Skip("promtool unit tests cannot evaluate mad_over_time: prometheus/prometheus#20046")
			}

			detection := tt.band
			detection.Window = v1.NewDurationShorthand(
				int(tt.window.Minutes()), v1.DurationShorthandUnitMinute)
			detector := parityDetector(detection)

			band, err := detector.Band(tt.step)
			if err != nil {
				t.Fatalf("Band: %v", err)
			}

			// The first instant with a complete window at every offset, and
			// the last one asserted. Nothing before the lookback has a band
			// in either implementation.
			from := band.Lookback()
			to := from + tt.span

			// One declaration read by both sides: the values
			// internal/detect sees and the promtool notation for the same
			// numbers. Two hand-written copies of three weeks of samples
			// would be two things to keep in step.
			values, notation := rising(10, 1, int(to/tt.step))

			results, err := detect.Evaluate(band, detect.Range{
				Start: epoch(from),
				End:   epoch(to),
				Step:  tt.step,
			}, points(values, tt.step))
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if len(results) == 0 {
				t.Fatal("no instants to compare")
			}
			for _, result := range results {
				if result.Status != detect.Evaluated {
					t.Fatalf("status %q at %s, want a band: the input series is too short",
						result.Status, result.At.Sub(epoch(0)))
				}
			}

			out, err := Emit(Target{ExperimentalFunctions: tt.mad}, tt.step,
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
			if err := os.WriteFile(unitFile, unitTest("rules.yml", notation, tt.step, results), 0o600); err != nil {
				t.Fatalf("writing the unit test: %v", err)
			}

			// The flag goes before the subcommand. promtool accepts it in
			// either position and honours it for loading only, which is the
			// skip above.
			var args []string
			if tt.mad {
				args = append(args, "--enable-feature=promql-experimental-functions")
			}
			args = append(args, "test", "rules", unitFile)

			started := time.Now()
			output, err := exec.Command(promtool, args...).CombinedOutput()
			t.Logf("%d instants, %s of series at %s, window %s, %s in promtool",
				len(results), to, promDuration(tt.step), promDuration(tt.window),
				time.Since(started).Round(time.Millisecond))
			if err != nil {
				t.Errorf("the compiled rules disagree with internal/detect: %v\n%s", err, output)
			}
		})
	}
}

// TestSeasonalRobustIsRefused is the other half of the seasonal story. A
// median of a pool is not a function of the per-window medians, so there is
// no decomposition to emit, and the compiler refuses rather than emitting
// the pooled mean and calling it robust.
//
// It belongs next to the parity test because that test can only compare what
// compiles: a seasonal robust band quietly emitting an adaptive-shaped band
// would show up there as a suite with one fewer case in it.
func TestSeasonalRobustIsRefused(t *testing.T) {
	detection := spec.Detection{
		Strategy:    detect.Robust,
		RobustScale: detect.ScaleIQR,
		Sensitivity: detect.Balanced,
		Class:       spec.ClassRequests,
		Seasonality: []spec.Seasonality{spec.Daily},
		Window:      v1.NewDurationShorthand(30, v1.DurationShorthandUnitMinute),
	}

	_, err := Emit(Target{}, time.Minute,
		Source{Path: "parity.yaml", Specification: []byte("parity")}, parityDetector(detection))
	if err == nil {
		t.Fatal("Emit compiled a robust band over pooled windows")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("Emit error = %v, want %v", err, ErrUnsupported)
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
//
// The per-offset means and variances a seasonal band records are not
// asserted. internal/detect never forms a per-window statistic: it pools the
// samples and computes one mean and one deviation, so an expectation for a
// single window's variance would be arithmetic written here rather than
// either implementation's, which is the second implementation this test
// exists to avoid. What those series add up to is the centre and the scale,
// and both are asserted at every instant.
func unitTest(ruleFile, input string, step time.Duration, results []detect.Result) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "rule_files:\n  - %s\n\n", ruleFile)
	fmt.Fprintf(&b, "evaluation_interval: %s\n\n", promDuration(step))
	b.WriteString("tests:\n")
	fmt.Fprintf(&b, "  - interval: %s\n", promDuration(step))
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

// rising is count+1 samples climbing by delta, as values and as the promtool
// notation for the same numbers.
//
// A climbing series is deliberate for the seasonal cases. Every offset
// window sits at a different level, so the variance of the per-window means
// is a real quantity rather than zero, and the identity the compiler relies
// on is exercised rather than satisfied trivially.
func rising(start, delta float64, count int) ([]float64, string) {
	values := make([]float64, 0, count+1)
	for i := 0; i <= count; i++ {
		values = append(values, start+delta*float64(i))
	}
	return values, fmt.Sprintf("%s+%sx%d",
		strconv.FormatFloat(start, 'g', -1, 64),
		strconv.FormatFloat(delta, 'g', -1, 64), count)
}

// points puts the values on the same grid promtool evaluates on: one sample
// per step from the epoch, which is where a promtool unit test starts.
func points(values []float64, step time.Duration) []prometheus.Point {
	out := make([]prometheus.Point, 0, len(values))
	for i, v := range values {
		out = append(out, prometheus.Point{At: epoch(time.Duration(i) * step), Value: v})
	}
	return out
}

func epoch(d time.Duration) time.Time { return time.Unix(0, 0).UTC().Add(d) }
