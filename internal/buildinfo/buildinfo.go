// Package buildinfo reports what a feno binary is: its version, the commit it
// was built from, and when.
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

// Version is stamped by the release build with
// `-ldflags "-X github.com/dennisme/feno/internal/buildinfo.Version=<tag>"`.
// It is the one symbol the release pipeline has to name, because everything
// else here comes from the toolchain. A build nobody stamped says dev.
var Version = "dev"

// Info is what `feno version` prints.
type Info struct {
	Version   string
	Commit    string
	BuildTime string
	Dirty     bool
	GoVersion string
}

// Get reads the facts the toolchain recorded into this binary.
func Get() Info {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return read(&debug.BuildInfo{})
	}
	return read(bi)
}

// read maps a BuildInfo onto Info. The toolchain records vcs.revision,
// vcs.time and vcs.modified for a `go build` inside a checkout and omits them
// everywhere else, test binaries included, so every field has a stand-in.
func read(bi *debug.BuildInfo) Info {
	info := Info{
		Version:   Version,
		Commit:    "unknown",
		BuildTime: "unknown",
		GoVersion: bi.GoVersion,
	}
	if info.GoVersion == "" {
		info.GoVersion = "unknown"
	}

	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Commit = s.Value
		case "vcs.time":
			info.BuildTime = s.Value
		case "vcs.modified":
			info.Dirty = s.Value == "true"
		}
	}
	return info
}

func (i Info) String() string {
	return fmt.Sprintf("feno %s\ncommit %s\nbuilt  %s\ndirty  %t\ngo     %s\n",
		i.Version, i.Commit, i.BuildTime, i.Dirty, i.GoVersion)
}
