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

// source is the provenance a generated file carries: the fixture path and
// the bytes behind it, read the same way the command reads them.
func source(t *testing.T, dir, name string) Source {
	t.Helper()
	path := filepath.Join("testdata", dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return Source{Path: path, Specification: data}
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
			out, err := Emit(tt.target, step, source(t, "detectors", tt.fixture), detectors...)
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

// TestUncompilable is the refusal path spec.md Part 9 asks to exercise while
// Prometheus is the only target. A refusal, not a warning: there is no
// weaker form of this band to emit, and no target that could compute one.
//
// The refusal is [detect.Spec.Validate]'s rather than this package's,
// because a robust band over several offset windows has no definition the
// backtest can evaluate either. What this asserts is that Emit reports it
// and names the detector it came from, since an author reading the error has
// a file to open.
func TestUncompilable(t *testing.T) {
	detectors := load(t, "uncompilable", "robust-seasonal.yaml")
	_, err := Emit(Target{ExperimentalFunctions: true}, step, Source{}, detectors...)
	if !errors.Is(err, detect.ErrRobustOffsets) {
		t.Fatalf("got %v, want %v", err, detect.ErrRobustOffsets)
	}
	if want := "checkout-errors"; !strings.Contains(err.Error(), want) {
		t.Errorf("error does not name %q: %v", want, err)
	}
}

// TestEqualWindowsAgreeWithOneSampleSet is why the parity cases without a
// gap in them cannot tell one weighting from another.
//
// The band weighs every offset window the same: the mean of the per-window
// means, and the mean of their variances plus the variance of those means.
// Where the windows are all the same length that is exactly the mean and the
// population variance over all the samples at once, so a case with full
// windows would pass whichever weighting either side used. It takes a window
// of a different length to tell them apart, which is what the gap cases in
// evaluated_test.go are for.
func TestEqualWindowsAgreeWithOneSampleSet(t *testing.T) {
	windows := [][]float64{
		{1, 2, 3, 4},
		{10, 12, 14, 16},
		{-5, 0, 5, 100},
	}

	var every []float64
	for _, w := range windows {
		every = append(every, w...)
	}

	means := make([]float64, len(windows))
	variances := make([]float64, len(windows))
	for i, w := range windows {
		means[i] = mean(w)
		variances[i] = variance(w, means[i])
	}
	centre := mean(means)
	combined := mean(variances) + variance(means, centre)

	if got, want := centre, mean(every); math.Abs(got-want) > 1e-9 {
		t.Errorf("centre: per window %v, over every sample %v", got, want)
	}
	if want := variance(every, mean(every)); math.Abs(combined-want) > 1e-9 {
		t.Errorf("variance: per window %v, over every sample %v", combined, want)
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
	out, err := Emit(Target{}, step, Source{}, detectors...)
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
			_, err := Emit(Target{}, step, Source{}, tt.detectors...)
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
// the real binary over every golden file and asserts nothing of its own. It
// skips when promtool is absent, because `just check` and CI run the pinned
// one and a contributor reading the tests should not need it to run them.
func TestPromtool(t *testing.T) {
	path, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("promtool not installed; `just init` installs the pinned version")
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
			// The experimental functions are enabled because that is the
			// server these files were compiled for: mad_over_time is only
			// emitted when the target declares it, and a check without the
			// feature would reject the file for the one reason the
			// compiler already accounted for.
			out, err := exec.Command(path, "check", "rules",
				"--enable-feature=promql-experimental-functions", file).CombinedOutput()
			if err != nil {
				t.Errorf("promtool rejected %s: %v\n%s", file, err, out)
			}
		})
	}
}
