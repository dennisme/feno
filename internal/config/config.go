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
	"os"
	"path/filepath"

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
}

// Unset reports limits that hold a detector to nothing.
func (l Limits) Unset() bool { return l.MaxEpisodes == nil && l.MaxFiringRatio == nil }

// For is the limits a detector is held to: the repository's, with each field
// the file names for this detector in place of it.
//
// Merging field by field rather than replacing the whole set is what makes a
// detector entry readable as the exception it is. A detector that needs a
// higher episode count says so in one line and stays under the repository's
// firing ratio.
//
// A detector the file does not name gets the repository's limits, and a
// detector named by a budget that no loaded specification declares is not an
// error: the specifications a backtest was pointed at are often a subset of
// the ones a repository holds.
func (b Budget) For(detector string) Limits {
	limits := b.Limits
	override, ok := b.Detectors[detector]
	if !ok {
		return limits
	}
	if override.MaxEpisodes != nil {
		limits.MaxEpisodes = override.MaxEpisodes
	}
	if override.MaxFiringRatio != nil {
		limits.MaxFiringRatio = override.MaxFiringRatio
	}
	return limits
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
	for name, limits := range f.Budget.Detectors {
		if err := limits.validate(fmt.Sprintf("budget.detectors.%s", name)); err != nil {
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

// Find walks up from a path looking for the nearest feno.yaml, and reports
// where it found one.
//
// The walk starts at the path when it is a directory and beside it when it is
// a file, so pointing a command at `detectors/checkout.yaml` and at
// `detectors/` look for the same budget. It climbs because the file sits
// above the specifications it governs: `alerting/feno.yaml` governs
// `alerting/detectors/`. Climbing rather than reading the working directory
// is what stops a budget being dropped by running the command from
// somewhere else in the repository.
//
// The nearest file wins, which lets one repository hold a budget per
// specification tree. The path comes back absolute, because the walk left
// the directory the caller named and a relative path out of it reads as a
// chain of parents rather than as a place.
func Find(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		dir = filepath.Dir(dir)
	}

	for {
		candidate := filepath.Join(dir, Filename)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}
