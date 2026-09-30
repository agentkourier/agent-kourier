package main

import (
	"bytes"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr bool
	}{
		{name: "version flag prints version", args: []string{"--version"}, wantCode: 0, wantStdout: "v1.2.3\n"},
		{name: "no args prints nothing", args: nil, wantCode: 0},
		{name: "unknown flag fails", args: []string{"--nope"}, wantCode: 2, wantStderr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, "v1.2.3", &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if got := stdout.String(); got != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
			}
			if gotErr := stderr.Len() > 0; gotErr != tt.wantStderr {
				t.Errorf("stderr non-empty = %v, want %v (%q)", gotErr, tt.wantStderr, stderr.String())
			}
		})
	}
}
