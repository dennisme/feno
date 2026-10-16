// Package docs holds the test that keeps the reference pages honest.
//
// There is no non-test file here on purpose. Nothing imports this and
// nothing compiles into the binary: the only thing the package exists for is
// to fail when a field somebody can write in a document is not written down
// anywhere a reader can find it.
package docs

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dennisme/feno/internal/config"
	"github.com/dennisme/feno/internal/spec"
)

// pages is the field a reader has in hand and the page that has to explain
// it. The split is the pages', not the packages': a detector's fields are on
// the page about writing a detector whichever package declares them.
var pages = []struct {
	path  string
	types []any
}{
	{
		path: "detectors.md",
		types: []any{
			spec.AnomalyDetector{},
			spec.DetectorSpec{},
			spec.Detection{},
			spec.Alerting{},
		},
	},
	{
		path: "feno-yaml.md",
		types: []any{
			config.File{},
			config.Target{},
			config.Budget{},
			config.Limits{},
			config.Waiver{},
		},
	},
}

// TestEveryFieldIsDocumented is the gate the docs are behind. A new field in
// one of the types above fails here until the page names it, which is the
// same bargain `promtool check rules` makes in `just check`: the
// documentation is verified rather than hoped for.
//
// The check is that the page writes the field's own name in code marks. It is
// the weakest claim that cannot pass by accident, and it deliberately says
// nothing about whether the prose around it is any good: a test cannot read.
func TestEveryFieldIsDocumented(t *testing.T) {
	for _, page := range pages {
		t.Run(page.path, func(t *testing.T) {
			text, err := os.ReadFile(filepath.Join("..", "..", "docs", page.path))
			if err != nil {
				t.Fatalf("reading the page: %v", err)
			}
			prose := string(text)

			for _, typ := range page.types {
				rt := reflect.TypeOf(typ)
				for _, field := range authoredFields(rt) {
					if !strings.Contains(prose, "`"+field+"`") {
						t.Errorf("%s.%s is authored in YAML and docs/%s does not name it",
							rt.Name(), field, page.path)
					}
				}
			}
		})
	}
}

// authoredFields is every name a document can carry for a type, which is its
// json tags and the tags of anything it embeds. A field tagged "-" is filled
// by the loader rather than written by an author, so it is not a field
// anybody can look up.
//
// A named struct field is not descended into. Which page its own fields
// belong on is a decision in [pages], and a type reached through two fields
// would otherwise be demanded twice.
func authoredFields(rt reflect.Type) []string {
	var names []string
	for i := range rt.NumField() {
		field := rt.Field(i)
		if !field.IsExported() {
			continue
		}
		if field.Anonymous {
			names = append(names, authoredFields(field.Type)...)
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	return names
}
