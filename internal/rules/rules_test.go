package rules

import (
	"errors"
	"flag"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"

	"github.com/dennisme/feno/internal/detect"
	"github.com/dennisme/feno/internal/spec"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// step is the evaluation interval every test compiles at. One minute is what
// a Prometheus runs by default, and it divides every fixture's window.
const step = time.Minute

func load(t *testing.T, dir string, names ...string) []spec.Detector {
	t.Helper()
	paths := make([]string, 0, len(names)+1)
	for _, name := range names {
		paths = append(paths, filepath.Join("testdata", dir, name))
	}
	paths = append(paths, filepath.Join("testdata", "detectors", "datasource.yaml"))
	bundle, err := spec.Load(paths...)
	if err != nil {
		t.Fatalf("loading %v: %v", names, err)
	}
	return bundle.Detectors
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v (run with -update)", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s does not match:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// TestGolden is the end of the slice: a detector YAML loads through
// internal/spec and compiles into a rule file.
func TestGolden(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		target   Target
		degraded bool
	}{
		{
			name:    "adaptive-plain.yml",
			fixture: "adaptive-plain.yaml",
		},
		{
			name:    "adaptive-seasonal.yml",
			fixture: "adaptive-seasonal.yaml",
		},
		{
			// Asks for mad_over_time on a server that has it.
			name:    "robust-mad.yml",
			fixture: "robust-plain.yaml",
			target:  Target{ExperimentalFunctions: true},
		},
		{
			// The same specification where the server does not, which is the
			// interquartile range and a declared degradation rather than a
			// silent substitution.
			name:     "robust-iqr.yml",
			fixture:  "robust-plain.yaml",
			degraded: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detectors := load(t, "detectors", tt.fixture)
			out, err := Emit(tt.target, step, detectors...)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			golden(t, tt.name, out.Rules)

			if tt.degraded {
				if len(out.Degradations) != 1 {
					t.Fatalf("got %d degradations, want 1: %v", len(out.Degradations), out.Degradations)
				}
				if got := out.Degradations[0].Requirement; got != detect.RequireMAD {
					t.Errorf("degradation requirement: got %q, want %q", got, detect.RequireMAD)
				}
			} else if len(out.Degradations) != 0 {
				t.Errorf("got degradations %v, want none", out.Degradations)
			}
		})
	}
}

// TestUncompilable is the degradation path spec.md Part 9 asks to exercise
// while Prometheus is the only target. A refusal, not a warning: there is no
// weaker form of this band to emit.
func TestUncompilable(t *testing.T) {
	detectors := load(t, "uncompilable", "robust-seasonal.yaml")
	_, err := Emit(Target{ExperimentalFunctions: true}, step, detectors...)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v, want %v", err, ErrUnsupported)
	}
	for _, want := range []string{"checkout-errors", string(detect.RequireSeasonalDecomposition)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

// TestPooledVarianceDecomposes is the check behind the pooled scale
// expression. The emitted rule computes the mean of the per-window variances
// plus the variance of their means, and that has to be the population
// variance over the pool internal/detect takes, not an approximation of it.
func TestPooledVarianceDecomposes(t *testing.T) {
	windows := [][]float64{
		{1, 2, 3, 4},
		{10, 12, 14, 16},
		{-5, 0, 5, 100},
	}

	var pool []float64
	for _, w := range windows {
		pool = append(pool, w...)
	}

	means := make([]float64, len(windows))
	variances := make([]float64, len(windows))
	for i, w := range windows {
		means[i] = mean(w)
		variances[i] = variance(w, means[i])
	}
	centre := mean(means)
	decomposed := mean(variances) + variance(means, centre)

	if got, want := centre, mean(pool); math.Abs(got-want) > 1e-9 {
		t.Errorf("pooled centre: decomposed %v, direct %v", got, want)
	}
	if want := variance(pool, mean(pool)); math.Abs(decomposed-want) > 1e-9 {
		t.Errorf("pooled variance: decomposed %v, direct %v", decomposed, want)
	}
}

func mean(vs []float64) float64 {
	var sum float64
	for _, v := range vs {
		sum += v
	}
	return sum / float64(len(vs))
}

func variance(vs []float64, m float64) float64 {
	var sum float64
	for _, v := range vs {
		d := v - m
		sum += d * d
	}
	return sum / float64(len(vs))
}

// TestWindowBoundary pins the two things that keep the generated rule and
// the backtest reading the same samples: the range is exactly Spec.Window,
// and an offset is the exact duration in Spec.Offsets rather than the
// reference library's period minus half a window.
func TestWindowBoundary(t *testing.T) {
	detectors := load(t, "detectors", "adaptive-seasonal.yaml")
	band, err := detectors[0].Band(step)
	if err != nil {
		t.Fatalf("Band: %v", err)
	}
	out, err := Emit(Target{}, step, detectors...)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	rules := string(out.Rules)

	if want := "[" + promDuration(band.Window) + "]"; !strings.Contains(rules, want) {
		t.Errorf("no range selector %q in the output", want)
	}
	for _, offset := range band.Offsets {
		if want := "offset " + promDuration(offset); !strings.Contains(rules, want) {
			t.Errorf("no %q in the output", want)
		}
	}
	// 167h30m is the reference library's centred offset. Emitting it would
	// mean a window the backtest never reads.
	if strings.Contains(rules, "167h30m") {
		t.Error("output carries the reference library's centred offset")
	}
}

func TestEmitErrors(t *testing.T) {
	detector := func(name, sourceType, query string) spec.Detector {
		source := v1.SLIMetricSource{Type: sourceType}
		if query != "" {
			source.Spec = map[string]any{"query": query}
		}
		return spec.Detector{
			AnomalyDetector: spec.AnomalyDetector{
				APIVersion: spec.APIVersion,
				Kind:       spec.Kind,
				Metadata:   v1.Metadata{Name: name},
				Spec: spec.DetectorSpec{
					Indicator: v1.SLOIndicatorInline{
						Metadata: v1.Metadata{Name: "i"},
						Spec: v1.SLISpec{
							ThresholdMetric: &v1.SLIMetricSpec{MetricSource: source},
						},
					},
					Detection: spec.Detection{
						Strategy:    detect.Adaptive,
						Sensitivity: detect.Balanced,
						Class:       spec.ClassRequests,
					},
				},
			},
		}
	}

	tests := []struct {
		name      string
		detectors []spec.Detector
		want      error
		mentions  []string
	}{
		{
			// Hyphens are idiomatic in a DNS label and illegal in a metric
			// name, so they are rewritten, and the rewrite can collide.
			name: "two names compile to one series",
			detectors: []spec.Detector{
				detector("checkout-rate", SourceType, "up"),
				detector("checkout_rate", SourceType, "up"),
			},
			want:     ErrName,
			mentions: []string{"checkout-rate", "checkout_rate", "checkout_rate"},
		},
		{
			name:      "a name no rewrite rescues",
			detectors: []spec.Detector{detector("checkout.rate", SourceType, "up")},
			want:      ErrName,
			mentions:  []string{"checkout.rate"},
		},
		{
			name:      "a datasource that does not speak PromQL",
			detectors: []spec.Detector{detector("checkout-rate", "Datadog", "up")},
			want:      ErrSourceType,
			mentions:  []string{"checkout-rate", "Datadog"},
		},
		{
			// internal/spec rejects a metric source with no spec at all at
			// load time; this is a spec that has one and no PromQL in it,
			// which only this target can judge.
			name:      "no query for this target",
			detectors: []spec.Detector{detector("checkout-rate", SourceType, "")},
			want:      ErrQuery,
			mentions:  []string{"checkout-rate"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Emit(Target{}, step, tt.detectors...)
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			for _, want := range tt.mentions {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not name %q: %v", want, err)
				}
			}
		})
	}
}

func TestPromDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Second, "30s"},
		{time.Minute, "1m"},
		{time.Hour, "1h"},
		{90 * time.Minute, "1h30m"},
		{24 * time.Hour, "1d"},
		{7 * 24 * time.Hour, "7d"},
		{3 * 7 * 24 * time.Hour, "21d"},
		{25*time.Hour + 30*time.Minute, "1d1h30m"},
	}
	for _, tt := range tests {
		if got := promDuration(tt.in); got != tt.want {
			t.Errorf("promDuration(%v): got %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestPromtool is the free verification spec.md Part 9 counts on. It runs
// the real binary over every golden file and asserts nothing of its own, so
// it skips rather than passing when promtool is absent: `promtool` is not in
// .tool-versions, and installing a tool is not this test's decision.
func TestPromtool(t *testing.T) {
	path, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("promtool not installed and not in .tool-versions; `just check` cannot verify the generated rules")
	}

	files, err := filepath.Glob(filepath.Join("testdata", "golden", "*.yml"))
	if err != nil {
		t.Fatalf("globbing golden files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no golden files to check")
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			out, err := exec.Command(path, "check", "rules", file).CombinedOutput()
			if err != nil {
				t.Errorf("promtool rejected %s: %v\n%s", file, err, out)
			}
		})
	}
}
