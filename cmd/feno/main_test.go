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
			args:     []string{"generate"},
			want:     exitUsage,
			inStderr: `unknown command "generate"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			got := run(tt.args, &stdout, &stderr)
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
