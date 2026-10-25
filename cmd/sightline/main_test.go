package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no command shows usage", args: nil, wantCode: 2, wantStderr: "Usage:"},
		{name: "unknown command", args: []string{"nope"}, wantCode: 2, wantStderr: `unknown command "nope"`},
		{name: "help", args: []string{"help"}, wantCode: 0, wantStdout: "Usage:"},
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "sightline dev"},
		{
			name:       "watch reports the pane herdr passed in",
			args:       []string{"watch"},
			env:        map[string]string{"HERDR_PANE_ID": "w1:p2"},
			wantCode:   0,
			wantStdout: "pane w1:p2",
		},
		{
			name:       "unwatch without a pane still behaves",
			args:       []string{"unwatch"},
			wantCode:   0,
			wantStdout: "(no pane in context)",
		},
		{name: "feed closes on enter", args: []string{"feed"}, wantCode: 0, wantStdout: "Press enter to close."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			getenv := func(k string) string { return tt.env[k] }

			code := run(tt.args, getenv, strings.NewReader("\n"), &stdout, &stderr)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
