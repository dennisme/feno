package buildinfo

import (
	"runtime/debug"
	"strings"
	"testing"
)

// A test binary carries no vcs record, so every field read from the toolchain
// has to have a stand-in rather than an empty string.
func TestGetFillsEveryField(t *testing.T) {
	got := Get()

	for name, value := range map[string]string{
		"Version":   got.Version,
		"Commit":    got.Commit,
		"BuildTime": got.BuildTime,
		"GoVersion": got.GoVersion,
	} {
		if value == "" {
			t.Errorf("%s is empty", name)
		}
	}
}

func TestReadTakesTheVCSRecord(t *testing.T) {
	got := read(&debug.BuildInfo{
		GoVersion: "go1.26.8",
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.time", Value: "2026-10-10T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	})

	if got.Commit != "abc123" {
		t.Errorf("Commit = %q, want abc123", got.Commit)
	}
	if got.BuildTime != "2026-10-10T00:00:00Z" {
		t.Errorf("BuildTime = %q, want 2026-10-10T00:00:00Z", got.BuildTime)
	}
	if !got.Dirty {
		t.Error("Dirty = false, want true")
	}
	if got.GoVersion != "go1.26.8" {
		t.Errorf("GoVersion = %q, want go1.26.8", got.GoVersion)
	}
}

// The string is what `feno version` prints, so it names the binary.
func TestStringNamesTheBinary(t *testing.T) {
	got := Info{Version: "v1.2.3"}.String()

	if !strings.HasPrefix(got, "feno v1.2.3\n") {
		t.Errorf("String() = %q, want it to start with the binary and the version", got)
	}
}
