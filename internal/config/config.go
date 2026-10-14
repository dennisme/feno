// Package config reads feno.yaml, the file beside a repository's
// specifications that carries its targets and its noise budget.
//
// # Why the budget is a file and not a flag
//
// A noise budget is the number that decides whether a detector reaches
// production, and a number somebody can drop by editing the workflow that
// checks it is not a limit, it is a suggestion. The same argument puts check
// severities in a policy file rather than in a command line. So the budget
// lives in a file committed next to the specifications it governs, where
// changing it is a reviewed diff in the repository that owns the detectors.
//
// # What is here and what stays a flag
//
// A target is what the repository decided once: which engine the rules are
// for and which directory they are written to. A URL, a header and a step are
// not that. A URL is where a particular runner can reach a particular server,
// a header carries a tenant or a credential, and neither belongs in a
// committed file. They stay flags.
//
// # Unset is not zero
//
// Every limit is a pointer. Zero is a budget somebody means, for a detector
// that may never page, so the absent value and the zero value have to be
// different things. [Limits.Unset] is the question "did anybody ask for a
// budget here", which is what decides whether a missing feno.yaml matters.
package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"

	"sigs.k8s.io/yaml"
)

// Filename is what the file is called, in every repository. It is not
// configurable: the point of the file is that a reviewer knows where to look
// for the budget without being told.
const Filename = "feno.yaml"

var (
	// ErrDecode reports a file that does not decode. An unknown field is
	// part of this rather than something to ignore, because a misspelled
	// `maxEpisodes` would otherwise be a budget that was written down and
	// never enforced.
	ErrDecode = errors.New("cannot decode " + Filename)

	// ErrValue reports a field whose value is outside what it can mean: a
	// negative count, a ratio that is not a fraction, a target with no
	// output directory.
	ErrValue = errors.New("invalid value")

	// ErrDuplicateEngine reports two targets for one engine. Taking either
	// one would make the output directory depend on the order the file
	// happened to list them in.
	ErrDuplicateEngine = errors.New("duplicate target engine")
)

// File is one feno.yaml, as authored.
type File struct {
	Targets []Target `json:"targets,omitempty"`
	Budget  Budget   `json:"budget,omitempty"`

	// Path is where the file was read from, so that an error about a budget
	// can name the file that set it.
	Path string `json:"-"`
}

// Target is where the rules for one engine are written. These are the two
// `generate` flags that are facts about the repository rather than about the
// machine the command runs on.
type Target struct {
	Engine string `json:"engine"`
	Output string `json:"output"`
}

// Target is the target for an engine, and whether the file declares one.
//
// The engine name is not validated here. Which engines exist is the
// knowledge of whatever compiles for them, and a file naming an engine this
// build has no target for is refused by the command that was asked for it,
// with the list of what it does have.
func (f File) Target(engine string) (Target, bool) {
	for _, target := range f.Targets {
		if target.Engine == engine {
			return target, true
		}
	}
	return Target{}, false
}

// Budget is the noise budget: limits for the repository, and the detectors
// that need their own.
type Budget struct {
	Limits
	Detectors map[string]Limits `json:"detectors,omitempty"`
}

// Limits is what a detector is held to over a backtest's window.
type Limits struct {
	// MaxEpisodes is how many times a detector may have paged. Nil is no
	// limit.
	MaxEpisodes *int `json:"maxEpisodes,omitempty"`

	// MaxFiringRatio is the fraction of the window a detector may have been
	// paging for, between zero and one. Nil is no limit.
	MaxFiringRatio *float64 `json:"maxFiringRatio,omitempty"`

	// EpisodesFrom and RatioFrom are the file each limit came from, filled
	// by resolution and empty on a file as decoded. A finding names them,
	// because "over budget" is not actionable until a reader knows which
	// file set the budget.
	//
	// There is no line number. The YAML here is decoded through
	// encoding/json, which does not carry one, and a second parser to
	// report it is more than the message is worth.
	EpisodesFrom string `json:"-"`
	RatioFrom    string `json:"-"`
}

// Unset reports limits that hold a detector to nothing.
func (l Limits) Unset() bool { return l.MaxEpisodes == nil && l.MaxFiringRatio == nil }

// For is the limits one file holds a detector to: this file's defaults and
// its entry for this detector, whichever of the two is stricter in each
// field.
//
// Stricter binds everywhere, so the rule is one rule. A detector entry is how
// a known-chatty detector is held tighter than its neighbours, and a looser
// entry is refused at load rather than resolved here, because inside one file
// a limit that can never apply is a mistake its author can see.
//
// A detector the file does not name gets the file's defaults, and a detector
// named by a budget that no loaded specification declares is not an error:
// the specifications a backtest was pointed at are often a subset of the ones
// a repository holds.
func (b Budget) For(detector string) Limits {
	return b.tighten(b.Detectors[detector])
}

// Load reads one feno.yaml.
//
// A file that is not there is an error, because the only way to get here is
// to have asked for this path by name. Whether a repository has to have one
// at all is [Find]'s question.
func Load(path string) (File, error) {
	// G304 is reading a file whose path came from a variable, which is what
	// a loader over a repository the operator named does.
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return File{}, fmt.Errorf("reading %s: %w", path, err)
	}

	var file File
	if err := yaml.UnmarshalStrict(data, &file); err != nil {
		return File{}, fmt.Errorf("%s: %w: %w", path, ErrDecode, err)
	}
	file.Path = path

	if err := file.validate(); err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	return file, nil
}

// validate rejects what decodes and cannot mean anything.
func (f File) validate() error {
	engines := map[string]int{}
	for i, target := range f.Targets {
		if target.Engine == "" {
			return fmt.Errorf("targets[%d].engine is required: %w", i, ErrValue)
		}
		if target.Output == "" {
			return fmt.Errorf("targets[%d].output is required: %w", i, ErrValue)
		}
		if first, ok := engines[target.Engine]; ok {
			return fmt.Errorf("targets[%d].engine %q is already the engine of targets[%d]: %w",
				i, target.Engine, first, ErrDuplicateEngine)
		}
		engines[target.Engine] = i
	}

	if err := f.Budget.validate("budget"); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(f.Budget.Detectors)) {
		limits := f.Budget.Detectors[name]
		field := fmt.Sprintf("budget.detectors.%s", name)
		if err := limits.validate(field); err != nil {
			return err
		}
		if err := limits.looser(f.Budget.Limits, field); err != nil {
			return err
		}
	}
	return nil
}

// validate rejects a limit that is not a limit. The field path is passed in
// so that the message names where in the file the number was written.
func (l Limits) validate(field string) error {
	if l.MaxEpisodes != nil && *l.MaxEpisodes < 0 {
		return fmt.Errorf("%s.maxEpisodes is %d, which is not a count of pages: %w",
			field, *l.MaxEpisodes, ErrValue)
	}
	if l.MaxFiringRatio != nil && (*l.MaxFiringRatio < 0 || *l.MaxFiringRatio > 1) {
		return fmt.Errorf("%s.maxFiringRatio is %v, which is not a fraction of the window: %w",
			field, *l.MaxFiringRatio, ErrValue)
	}
	return nil
}

// regularFile reports a path that exists and is a file, which is the only
// thing a feno.yaml can be.
func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
