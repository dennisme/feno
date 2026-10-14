package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/OpenSLO/go-sdk/pkg/openslo"
	v1 "github.com/OpenSLO/go-sdk/pkg/openslo/v1"
	"github.com/OpenSLO/go-sdk/pkg/openslosdk"
	"sigs.k8s.io/yaml"

	"github.com/dennisme/feno/internal/config"
)

var (
	// ErrUnknownKind reports a document in neither this group nor OpenSLO's.
	// It is an error rather than a document to skip: a directory passed to
	// this tool is a directory of specifications, and silently ignoring one
	// because its apiVersion has a typo in it is how a detector goes missing
	// without anybody noticing it was never generated.
	ErrUnknownKind = errors.New("unknown apiVersion and kind")

	// ErrDuplicateName reports two documents of the same kind sharing a
	// name. Names are how a metricSourceRef resolves and how a generated
	// rule group is identified, so a duplicate makes both ambiguous. Taking
	// the last one would make the result depend on the order the filesystem
	// returned the files in.
	ErrDuplicateName = errors.New("duplicate name")

	// ErrUnresolvedRef reports a metricSourceRef naming a DataSource that no
	// loaded document declares. Falling back to the metric source's own
	// type would mean a detector whose reference was never satisfied still
	// compiling, against whatever connection the target happened to default
	// to.
	ErrUnresolvedRef = errors.New("metricSourceRef names no DataSource")

	// ErrRefKind reports a metricSourceRef naming a document that exists and
	// is not a DataSource.
	ErrRefKind = errors.New("metricSourceRef does not name a DataSource")

	// ErrNoFiles reports a path that held no YAML.
	ErrNoFiles = errors.New("no YAML files found")

	// ErrDecode reports a document in this group that does not decode. An
	// unknown field is part of this rather than something to ignore: a
	// misspelled `seasonality` would otherwise generate a band with no
	// seasonality at all and no sign that any was asked for.
	ErrDecode = errors.New("cannot decode detector")
)

// Detector is a loaded [AnomalyDetector]: the document as authored, the file
// it came from so that anything downstream can say where, and the DataSource
// its metricSourceRef named.
type Detector struct {
	AnomalyDetector

	// File is the path the document was read from.
	File string

	// DataSource is what metricSourceRef resolved to, and is nil when the
	// metric source declares its own type inline.
	DataSource *v1.DataSource
}

// SourceType is the datasource type the query runs against: the resolved
// DataSource's type when the detector referenced one, and the metric
// source's own type when it did not. Exactly one of the two is set, which
// [AnomalyDetector.Validate] is what guarantees.
func (d Detector) SourceType() string {
	if d.DataSource != nil {
		return d.DataSource.Spec.Type
	}
	source, _ := d.MetricSource()
	return source.Type
}

// Bundle is everything a set of paths declares.
//
// Objects is the OpenSLO half, decoded and validated by go-sdk and otherwise
// untouched. It is kept rather than discarded because the cheapest useful
// command in the tool derives a detector from an SLI somebody already wrote,
// and because dropping it would mean a directory that round-trips through
// this loader loses half its contents.
type Bundle struct {
	Detectors []Detector
	Objects   []openslo.Object
}

// Load reads every YAML document under the given paths, which may be files
// or directories.
//
// A directory is walked, so a repository that nests its specifications by
// team or by service is loaded by naming the tree rather than every leaf. It
// is also what the noise budget's scope is resolved against: internal/config
// walks up from a detector's own file and stops at the path named here, so a
// tree that cannot be named cannot have a budget above its leaves.
//
// Documents are dispatched on apiVersion and kind. OpenSLO's go to go-sdk's
// decoder and validator, as the bytes the author wrote. Ours are decoded
// here, strictly, so that a misspelled field is an error rather than a
// default. Names are then checked for duplicates across the whole set and
// every metricSourceRef is resolved, both of which need all the files and so
// cannot happen per document.
func Load(paths ...string) (Bundle, error) {
	files, err := collect(paths)
	if err != nil {
		return Bundle{}, err
	}

	var (
		bundle    Bundle
		seen      = map[string]string{} // kind/name -> file that declared it
		sources   = map[string]*v1.DataSource{}
		detectors []Detector
	)

	for _, file := range files {
		// G304 is reading a file whose path came from a variable, which is
		// what a loader over a directory the operator named does. There is
		// no path to constrain it to: specifications live wherever the
		// repository keeps them.
		data, err := os.ReadFile(file) //nolint:gosec
		if err != nil {
			return Bundle{}, fmt.Errorf("reading %s: %w", file, err)
		}
		for _, doc := range splitDocuments(file, data) {
			var head struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
			}
			if err := yaml.Unmarshal(doc.data, &head); err != nil {
				return Bundle{}, fmt.Errorf("%s: %w: %w", doc, ErrDecode, err)
			}

			switch {
			case head.APIVersion == APIVersion:
				if head.Kind != Kind {
					return Bundle{}, fmt.Errorf("%s: %q: %w", doc, head.Kind, ErrKind)
				}
				var d AnomalyDetector
				if err := yaml.UnmarshalStrict(doc.data, &d); err != nil {
					return Bundle{}, fmt.Errorf("%s: %w: %w", doc, ErrDecode, err)
				}
				if err := d.Validate(); err != nil {
					return Bundle{}, fmt.Errorf("%s: %w", doc, err)
				}
				if err := claim(seen, doc, d.Kind, d.Metadata.Name); err != nil {
					return Bundle{}, err
				}
				detectors = append(detectors, Detector{AnomalyDetector: d, File: file})

			case openslo.Version(head.APIVersion).Validate() == nil:
				objects, err := openslosdk.Decode(bytes.NewReader(doc.data), openslosdk.FormatYAML)
				if err != nil {
					return Bundle{}, fmt.Errorf("%s: %w", doc, err)
				}
				if err := openslosdk.Validate(objects...); err != nil {
					return Bundle{}, fmt.Errorf("%s: %w", doc, err)
				}
				for i := range objects {
					object := objects[i]
					if err := claim(seen, doc, object.GetKind().String(), object.GetName()); err != nil {
						return Bundle{}, err
					}
					if source, ok := object.(v1.DataSource); ok {
						sources[source.Metadata.Name] = &source
					}
				}
				bundle.Objects = append(bundle.Objects, objects...)

			default:
				return Bundle{}, fmt.Errorf("%s: apiVersion %q kind %q: %w",
					doc, head.APIVersion, head.Kind, ErrUnknownKind)
			}
		}
	}

	for i := range detectors {
		source, err := detectors[i].MetricSource()
		if err != nil {
			return Bundle{}, fmt.Errorf("%s: %w", detectors[i].File, err)
		}
		ref := source.MetricSourceRef
		if ref == "" {
			continue
		}
		dataSource, ok := sources[ref]
		if !ok {
			// A name that exists under some other kind is a different
			// mistake from a name that exists nowhere, and the author fixes
			// the two differently, so they are reported differently. The
			// keys are sorted because a map would otherwise name whichever
			// match it reached first.
			for _, key := range slices.Sorted(maps.Keys(seen)) {
				kind, name, _ := strings.Cut(key, "/")
				if name != ref {
					continue
				}
				return Bundle{}, fmt.Errorf("%s: detector %q references %q, which is a %s declared in %s: %w",
					detectors[i].File, detectors[i].Metadata.Name, ref, kind, seen[key], ErrRefKind)
			}
			return Bundle{}, fmt.Errorf("%s: detector %q references %q: %w",
				detectors[i].File, detectors[i].Metadata.Name, ref, ErrUnresolvedRef)
		}
		detectors[i].DataSource = dataSource
	}

	bundle.Detectors = detectors
	return bundle, nil
}

// claim records that a kind and name were declared, and reports a name
// already taken along with both files.
func claim(seen map[string]string, doc document, kind, name string) error {
	key := kind + "/" + name
	if where, ok := seen[key]; ok {
		return fmt.Errorf("%s: %s %q already declared in %s: %w",
			doc, kind, name, where, ErrDuplicateName)
	}
	seen[key] = doc.String()
	return nil
}

// collect expands paths into the YAML files to read, in a stable order so
// that an error names the same file every run.
func collect(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, ErrNoFiles
	}
	var files []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, path)
			continue
		}
		walked, err := yamlUnder(path)
		if err != nil {
			return nil, err
		}
		files = append(files, walked...)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%v: %w", paths, ErrNoFiles)
	}
	slices.Sort(files)
	return files, nil
}

// yamlUnder is every YAML file in a tree.
//
// A directory whose name starts with a dot is skipped whole. The one that
// matters is .git, which holds enough YAML to make an error message about a
// specification point inside a packfile.
func yamlUnder(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		// feno.yaml is reserved in a specification tree: it is the targets
		// and the noise budget, read by internal/config, and a walk that
		// collected it would refuse the whole tree as an unknown kind.
		if isYAML(entry.Name()) && entry.Name() != config.Filename {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func isYAML(name string) bool {
	ext := filepath.Ext(name)
	return ext == ".yaml" || ext == ".yml"
}

// document is one YAML document, kept as the bytes the author wrote so that
// an OpenSLO document reaches go-sdk's decoder exactly as `oslo validate`
// would see it.
type document struct {
	file string
	line int
	data []byte
}

// String is the file and the line the document starts on, which is what an
// error leads with.
func (d document) String() string { return fmt.Sprintf("%s:%d", d.file, d.line) }

// splitDocuments splits a YAML stream into its documents.
//
// A separator is a line at column zero that is `---` on its own or starts
// `--- `. Column zero is what makes this safe rather than a guess: the
// content of a block scalar and the continuation lines of a plain scalar
// both have to be indented further than the key that owns them, so a `---`
// inside a multi-line query cannot sit at column zero. Matching the
// separator anywhere in the text instead, which is the usual shortcut, does
// split such a query in half.
//
// Empty documents are dropped, which is what a leading `---` produces.
func splitDocuments(file string, data []byte) []document {
	var (
		docs    []document
		current = document{file: file, line: 1}
		body    []byte
		line    = 1
	)
	flush := func() {
		if len(bytes.TrimSpace(body)) > 0 {
			current.data = body
			docs = append(docs, current)
		}
		body = nil
	}
	for _, raw := range bytes.SplitAfter(data, []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		text := strings.TrimRight(string(raw), "\r\n")
		if text == "---" || strings.HasPrefix(text, "--- ") {
			flush()
			current = document{file: file, line: line + 1}
			line++
			continue
		}
		body = append(body, raw...)
		line++
	}
	flush()
	return docs
}
