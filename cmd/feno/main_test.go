package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		want     int
		inStdout string
		inStderr string
	}{
		{
			name:     "no arguments prints usage",
			args:     nil,
			want:     exitUsage,
			inStderr: "usage: feno",
		},
		{
			name:     "version reports the build",
			args:     []string{"version"},
			want:     exitOK,
			inStdout: "feno dev",
		},
		{
			name:     "an unknown command names what it wanted",
			args:     []string{"serve"},
			want:     exitUsage,
			inStderr: `unknown command "serve"`,
		},
		{
			name:     "help asked for goes to stdout",
			args:     []string{"help"},
			want:     exitOK,
			inStdout: "usage: feno",
		},
		{
			name:     "--help asked for goes to stdout",
			args:     []string{"--help"},
			want:     exitOK,
			inStdout: "usage: feno",
		},
		{
			name:     "-h asked for goes to stdout",
			args:     []string{"-h"},
			want:     exitOK,
			inStdout: "usage: feno",
		},
		{
			name:     "the help names what each command is for",
			args:     []string{"--help"},
			want:     exitOK,
			inStdout: "replay detectors over history",
		},
		{
			name:     "the help says where the flags are",
			args:     []string{"--help"},
			want:     exitOK,
			inStdout: "feno <command> --help",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			got := run(t.Context(), tt.args, &stdout, &stderr)
			if got != tt.want {
				t.Errorf("exit code = %d, want %d", got, tt.want)
			}
			if tt.inStdout != "" && !strings.Contains(stdout.String(), tt.inStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.inStdout)
			}
			if tt.inStderr != "" && !strings.Contains(stderr.String(), tt.inStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.inStderr)
			}
		})
	}
}

// Help asked for is output rather than an error, on every command that has
// any. A person who typed --help got what they asked for, so it goes to
// stdout and the run succeeds, and `feno backtest --help | less` shows
// something.
func TestSubcommandHelpIsOutput(t *testing.T) {
	for _, command := range []string{"generate", "backtest", "suggest"} {
		t.Run(command, func(t *testing.T) {
			for _, flag := range []string{"-h", "--help"} {
				var stdout, stderr bytes.Buffer

				got := run(t.Context(), []string{command, flag}, &stdout, &stderr)
				if got != exitOK {
					t.Errorf("%s %s: exit code = %d, want %d", command, flag, got, exitOK)
				}
				if want := "usage: feno " + command; !strings.Contains(stdout.String(), want) {
					t.Errorf("%s %s: stdout = %q, want it to contain %q", command, flag, stdout.String(), want)
				}
				if stderr.Len() != 0 {
					t.Errorf("%s %s: stderr = %q, want nothing", command, flag, stderr.String())
				}
			}
		})
	}
}

// A usage error nobody asked for keeps stderr and a non-zero code, which is
// the other half of the decision above.
func TestUsageErrorStaysOnStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer

	got := run(t.Context(), []string{"backtest", "-nosuchflag"}, &stdout, &stderr)
	if got != exitUsage {
		t.Errorf("exit code = %d, want %d", got, exitUsage)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
	for _, want := range []string{"not defined", "usage: feno backtest"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
}
