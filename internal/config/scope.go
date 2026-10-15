package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// Scope resolves the budget a detector is held to from the feno.yaml files
// above it.
//
// # Why resolution is per detector
//
// A repository with more than one team has a budget per team, and one
// invocation covers all of them. Resolving a single file for the whole run
// would hold every team to whichever file sat above the path the command was
// pointed at, which is the budget of whoever owns the top of the tree and
// nobody else.
//
// # Both apply, and the stricter binds
//
// Every file from the root of the walk down to the detector's own directory
// applies. A nested file neither overrides the one above it nor is overridden
// by it: each field resolves to the lower of the two. So a team may tighten
// what the repository set and may not loosen it, which is the same argument
// that put the budget in a file rather than in a workflow one level down.
//
// A team entry looser than the repository's is dead config rather than an
// error, because the two files have different authors and a repository-wide
// tightening should not break every team's build at once. Inside one file it
// is an error, which is [File.validate]: one author owns that file and can
// fix it.
//
// # Where the walk stops
//
// At the path the command was pointed at. A file above it was never asked
// for, and a budget that binds because of where somebody was standing is the
// thing this is trying not to be. Pointing at the tree is how a
// repository-wide ceiling is asked for, which is why internal/spec walks a
// directory rather than reading one level of it.
type Scope struct {
	// roots are the paths the command was pointed at, absolute and deepest
	// first, so the nearest enclosing one is found first.
	roots []string

	// files is one entry per directory looked in, nil when it held no
	// feno.yaml, so a tree is read once however many detectors came out of
	// it.
	files map[string]*File
}

// NewScope is the scope over the specification paths a command was given.
func NewScope(roots ...string) *Scope {
	scope := &Scope{files: map[string]*File{}}
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		// A root that names a file is the directory holding it: `-f
		// detectors/checkout.yaml` and `-f detectors/` are pointed at the
		// same tree.
		if filepath.Ext(absolute) != "" {
			absolute = filepath.Dir(absolute)
		}
		scope.roots = append(scope.roots, absolute)
	}
	// Deepest first, so a nested root wins over an enclosing one when a
	// command was given both.
	slices.SortFunc(scope.roots, func(a, b string) int { return len(b) - len(a) })
	return scope
}

// Limits is the budget a detector is held to, and the file each limit came
// from.
func (s *Scope) Limits(detectorFile, detector string) (Limits, error) {
	directory, err := filepath.Abs(filepath.Dir(detectorFile))
	if err != nil {
		return Limits{}, err
	}

	var limits Limits
	for _, dir := range s.walk(directory) {
		file, err := s.read(dir)
		if err != nil {
			return Limits{}, err
		}
		if file == nil {
			continue
		}
		limits = limits.tighten(file.LimitsFor(detector))
	}
	return limits, nil
}

// Files is every feno.yaml the scope has read, sorted, which is how a run
// says what it enforced and a run with no budget says that instead.
func (s *Scope) Files() []string {
	var out []string
	for _, file := range s.files {
		if file != nil {
			out = append(out, file.Path)
		}
	}
	slices.Sort(out)
	return out
}

// walk is the directories a detector's budget may come from, from the root of
// its tree down to its own directory.
//
// A detector under none of the roots is held to its own directory alone. That
// is not reachable through either command, which only ever asks about
// specifications it loaded from a root, and it keeps this from climbing to
// the filesystem root if a third caller does.
func (s *Scope) walk(directory string) []string {
	root := ""
	for _, candidate := range s.roots {
		if directory == candidate || strings.HasPrefix(directory, candidate+string(filepath.Separator)) {
			root = candidate
			break
		}
	}
	if root == "" {
		return []string{directory}
	}

	var dirs []string
	for dir := directory; ; dir = filepath.Dir(dir) {
		dirs = append(dirs, dir)
		if dir == root || dir == filepath.Dir(dir) {
			break
		}
	}
	slices.Reverse(dirs)
	return dirs
}

// read is the feno.yaml in a directory, nil when there is none, read once.
func (s *Scope) read(directory string) (*File, error) {
	if file, ok := s.files[directory]; ok {
		return file, nil
	}

	path := filepath.Join(directory, Filename)
	if !regularFile(path) {
		s.files[directory] = nil
		return nil, nil
	}
	file, err := Load(path)
	if err != nil {
		return nil, err
	}
	s.files[directory] = &file
	return &file, nil
}

// LimitsFor is the limits one file holds a detector to, carrying that file as
// the origin of each one and of any waiver.
func (f File) LimitsFor(detector string) Limits {
	limits := f.Budget.For(detector)
	if limits.MaxEpisodes != nil {
		limits.EpisodesFrom = f.Path
	}
	if limits.MaxFiringRatio != nil {
		limits.RatioFrom = f.Path
	}
	if waiver := f.Budget.Detectors[detector].Waiver; waiver != nil {
		// A copy, so that stamping the file onto it does not reach the
		// decoded file a scope holds for every other detector in the tree.
		stamped := *waiver
		stamped.From = f.Path
		limits.Waiver = &stamped
	}
	return limits
}

// tighten is the stricter of two limits, field by field, keeping the file
// that set whichever one binds.
//
// Equal values keep the origin already held, which is the file further up the
// tree: a limit a team restated is the repository's limit, and naming the
// repository's file is the more useful half of that.
func (l Limits) tighten(other Limits) Limits {
	out := l
	if other.MaxEpisodes != nil && (out.MaxEpisodes == nil || *other.MaxEpisodes < *out.MaxEpisodes) {
		out.MaxEpisodes = other.MaxEpisodes
		out.EpisodesFrom = other.EpisodesFrom
	}
	if other.MaxFiringRatio != nil && (out.MaxFiringRatio == nil || *other.MaxFiringRatio < *out.MaxFiringRatio) {
		out.MaxFiringRatio = other.MaxFiringRatio
		out.RatioFrom = other.RatioFrom
	}
	// A waiver is not a limit and "stricter" does not order it, so the
	// nearest file's waiver replaces the one above it. A team owns the
	// detector and the reason it is over budget, and what holds the decision
	// to an end is the expiry and the diff it was written in, not which file
	// it was written in.
	if other.Waiver != nil {
		out.Waiver = other.Waiver
	}
	return out
}

// looser reports a limit that can never apply because the same file already
// set a stricter one, and says so in the words the error takes.
func (l Limits) looser(stricter Limits, field string) error {
	if l.MaxEpisodes != nil && stricter.MaxEpisodes != nil && *l.MaxEpisodes > *stricter.MaxEpisodes {
		return fmt.Errorf("%s.maxEpisodes is %d, looser than the %d this file already sets, so it can never apply: %w",
			field, *l.MaxEpisodes, *stricter.MaxEpisodes, ErrValue)
	}
	if l.MaxFiringRatio != nil && stricter.MaxFiringRatio != nil && *l.MaxFiringRatio > *stricter.MaxFiringRatio {
		return fmt.Errorf("%s.maxFiringRatio is %v, looser than the %v this file already sets, so it can never apply: %w",
			field, *l.MaxFiringRatio, *stricter.MaxFiringRatio, ErrValue)
	}
	return nil
}

// Nearest is the feno.yaml closest to a path, and nothing when the walk from
// it holds none.
//
// It is what a command needs when one file has to answer for the whole run,
// the way a target does: a repository declares where its rules go once, and
// the nearest declaration to the specifications is that answer.
func (s *Scope) Nearest(path string) (*File, error) {
	directory, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if filepath.Ext(directory) != "" {
		directory = filepath.Dir(directory)
	}

	dirs := s.walk(directory)
	slices.Reverse(dirs)
	for _, dir := range dirs {
		file, err := s.read(dir)
		if err != nil {
			return nil, err
		}
		if file != nil {
			return file, nil
		}
	}
	return nil, nil
}
