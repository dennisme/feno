package spec

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenSLO/go-sdk/pkg/openslo"
	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"
	"github.com/OpenSLO/go-sdk/pkg/openslosdk"
)

func TestLoadMixedDirectory(t *testing.T) {
	bundle, err := Load(filepath.Join("testdata", "mixed"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(bundle.Detectors) != 2 {
		t.Fatalf("got %d detectors, want 2", len(bundle.Detectors))
	}
	if len(bundle.Objects) != 2 {
		t.Fatalf("got %d OpenSLO objects, want 2", len(bundle.Objects))
	}

	// The OpenSLO half keeps its own kinds and its own types, decoded by
	// go-sdk. Asserting the concrete types is the check that nothing here
	// rebuilt them.
	if _, ok := bundle.Objects[0].(v1.SLI); !ok {
		t.Errorf("first object is %T, want v1.SLI", bundle.Objects[0])
	}
	if _, ok := bundle.Objects[1].(v1.DataSource); !ok {
		t.Errorf("second object is %T, want v1.DataSource", bundle.Objects[1])
	}

	byName := map[string]Detector{}
	for _, d := range bundle.Detectors {
		byName[d.Metadata.Name] = d
	}

	checkout, ok := byName["checkout-request-rate"]
	if !ok {
		t.Fatalf("detector checkout-request-rate not loaded, got %v", byName)
	}
	if checkout.DataSource == nil {
		t.Fatal("checkout-request-rate: metricSourceRef did not resolve")
	}
	if got := checkout.DataSource.Metadata.Name; got != "mimir-prod" {
		t.Errorf("resolved DataSource: got %q, want %q", got, "mimir-prod")
	}
	// The type comes from the DataSource, which is the point of referencing
	// one: the detector never repeats it.
	if got := checkout.SourceType(); got != "Prometheus" {
		t.Errorf("SourceType: got %q, want %q", got, "Prometheus")
	}
	source, err := checkout.MetricSource()
	if err != nil {
		t.Fatalf("metricSource: %v", err)
	}
	// The query survived the block scalar that holds a line of three dashes.
	query, _ := source.Spec["query"].(string)
	if !strings.Contains(query, "---") {
		t.Errorf("query lost its block scalar content: %q", query)
	}
	if got := checkout.Spec.Alerting.For.String(); got != "10m" {
		t.Errorf("alerting.for: got %q, want %q", got, "10m")
	}
	if got := checkout.Spec.Detection.Class; got != ClassRequests {
		t.Errorf("class: got %q, want %q", got, ClassRequests)
	}
	if got := checkout.Metadata.Labels["owner"]; len(got) != 1 || got[0] != "payments-team" {
		t.Errorf("owner label: got %v, want [payments-team]", got)
	}

	inline, ok := byName["queue-depth"]
	if !ok {
		t.Fatalf("detector queue-depth not loaded, got %v", byName)
	}
	if inline.DataSource != nil {
		t.Errorf("queue-depth resolved a DataSource it never referenced: %v", inline.DataSource)
	}
	if got := inline.SourceType(); got != "Prometheus" {
		t.Errorf("SourceType: got %q, want %q", got, "Prometheus")
	}
}

// TestOpenSLOHalfStillValidates is the test behind the claim that one
// directory holds SLOs and detectors side by side and `oslo validate` still
// passes on the OpenSLO half.
//
// It asserts the library rather than running the CLI. `oslo` is a thin
// command over exactly the two calls below, Decode and Validate from
// go-sdk's openslosdk, and no part of this repository's toolchain installs
// it, so a test that shelled out to it would skip on every machine that
// matters and assert nothing. What it does assert is the part that could
// actually break here: that every OpenSLO document comes out of the splitter
// as the bytes the author wrote, with its neighbouring detector removed and
// nothing else changed.
func TestOpenSLOHalfStillValidates(t *testing.T) {
	root := filepath.Join("testdata", "mixed")
	files, err := collect([]string{root})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	var openSLODocs int
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		for _, doc := range splitDocuments(file, data) {
			if bytes.Contains(doc.data, []byte(APIVersion)) {
				continue
			}
			objects, err := openslosdk.Decode(bytes.NewReader(doc.data), openslosdk.FormatYAML)
			if err != nil {
				t.Errorf("%s: go-sdk rejected an OpenSLO document: %v", doc, err)
				continue
			}
			if err := openslosdk.Validate(objects...); err != nil {
				t.Errorf("%s: go-sdk failed to validate: %v", doc, err)
				continue
			}
			openSLODocs += len(objects)
		}
	}
	if openSLODocs != 2 {
		t.Errorf("validated %d OpenSLO objects, want 2", openSLODocs)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		want     error
		mentions []string
	}{
		{
			name: "metricSourceRef names nothing",
			path: "unresolved",
			want: ErrUnresolvedRef,
			// The error has to say which file and which name, because the
			// author is looking at a directory and not at one document.
			mentions: []string{"unresolved/detector.yaml", "checkout-request-rate", "mimir-staging"},
		},
		{
			name:     "two detectors share a name",
			path:     "duplicate",
			want:     ErrDuplicateName,
			mentions: []string{"duplicate/first.yaml", "duplicate/second.yaml", "checkout-request-rate"},
		},
		{
			name:     "apiVersion belongs to neither group",
			path:     "unknown",
			want:     ErrUnknownKind,
			mentions: []string{"unknown/detector.yaml", "anomaly.openslo.io/v1beta"},
		},
		{
			name:     "metricSourceRef names something that is not a DataSource",
			path:     "ref-wrong-kind",
			want:     ErrRefKind,
			mentions: []string{"ref-wrong-kind/specs.yaml", "mimir-prod", "Service"},
		},
		{
			name:     "our group with a kind it does not define",
			path:     "wrong-kind",
			want:     ErrKind,
			mentions: []string{"wrong-kind/detector.yaml", "AnomalyDetectors"},
		},
		{
			// A misspelled field is an error rather than a default, because
			// the alternative is a detector that generates rules with no
			// seasonality and no sign that any was asked for.
			name:     "a misspelled field",
			path:     "misspelled",
			want:     ErrDecode,
			mentions: []string{"misspelled/detector.yaml", "seasonalilty"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(filepath.Join("testdata", tt.path))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			got := filepath.ToSlash(err.Error())
			for _, want := range tt.mentions {
				if !strings.Contains(got, want) {
					t.Errorf("error does not name %q: %s", want, got)
				}
			}
		})
	}
}

func TestLoadNoFiles(t *testing.T) {
	if _, err := Load(); !errors.Is(err, ErrNoFiles) {
		t.Errorf("Load with no paths: got %v, want %v", err, ErrNoFiles)
	}
	if _, err := Load(t.TempDir()); !errors.Is(err, ErrNoFiles) {
		t.Errorf("Load of an empty directory: got %v, want %v", err, ErrNoFiles)
	}
}

func TestSplitDocuments(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []document
	}{
		{
			name:  "one document",
			input: "kind: SLI\n",
			want:  []document{{line: 1, data: []byte("kind: SLI\n")}},
		},
		{
			name:  "leading separator does not open an empty document",
			input: "---\nkind: SLI\n",
			want:  []document{{line: 2, data: []byte("kind: SLI\n")}},
		},
		{
			name:  "two documents report the line each starts on",
			input: "kind: SLI\n---\nkind: SLO\n",
			want: []document{
				{line: 1, data: []byte("kind: SLI\n")},
				{line: 3, data: []byte("kind: SLO\n")},
			},
		},
		{
			// Three dashes inside a block scalar are query text. Splitting
			// there would hand go-sdk half a document and a query with its
			// tail cut off.
			name:  "three dashes inside a block scalar are content",
			input: "query: |\n  a\n  ---\n  b\n",
			want:  []document{{line: 1, data: []byte("query: |\n  a\n  ---\n  b\n")}},
		},
		{
			name:  "a separator carrying a directive end marker still splits",
			input: "kind: SLI\n--- # second\nkind: SLO\n",
			want: []document{
				{line: 1, data: []byte("kind: SLI\n")},
				{line: 3, data: []byte("kind: SLO\n")},
			},
		},
		{
			name:  "trailing separator leaves no empty document",
			input: "kind: SLI\n---\n",
			want:  []document{{line: 1, data: []byte("kind: SLI\n")}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitDocuments("f.yaml", []byte(tt.input))
			if len(got) != len(tt.want) {
				t.Fatalf("got %d documents, want %d: %q", len(got), len(tt.want), got)
			}
			for i, want := range tt.want {
				if got[i].line != want.line {
					t.Errorf("document %d: line %d, want %d", i, got[i].line, want.line)
				}
				if !bytes.Equal(got[i].data, want.data) {
					t.Errorf("document %d: got %q, want %q", i, got[i].data, want.data)
				}
			}
		})
	}
}

// TestDetectorAPIVersionIsNotOpenSLOs guards the dispatch at its root: if our
// group ever became one go-sdk recognised, every detector would be handed to
// go-sdk's decoder and rejected as an unsupported kind.
func TestDetectorAPIVersionIsNotOpenSLOs(t *testing.T) {
	if err := openslo.Version(APIVersion).Validate(); err == nil {
		t.Errorf("go-sdk accepts %q as one of its own versions", APIVersion)
	}
}
