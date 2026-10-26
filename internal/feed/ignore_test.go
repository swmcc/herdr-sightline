package feed

import (
	"regexp"
	"strings"
	"testing"
)

// Real error lines, the rule suggested for each and a repeat of the same error
// with the details changed that the rule should still catch.
func TestSuggestRule(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		want   string
		repeat string // the same failure again, which the rule must match too
	}{
		{"node type error",
			"TypeError: Cannot read properties of undefined (reading 'map')",
			`^TypeError: Cannot read properties of undefined \(reading '[^']*'\)$`,
			"TypeError: Cannot read properties of undefined (reading 'filter')"},
		{"node missing module",
			"Error: Cannot find module '/app/src/lib/db'",
			`^Error: Cannot find module '[^']*'$`,
			"Error: Cannot find module '/app/src/lib/cache'"},
		{"node port in use",
			"Error: listen EADDRINUSE: address already in use :::3000",
			`^Error: listen EADDRINUSE: address already in use :::\d+$`,
			"Error: listen EADDRINUSE: address already in use :::5173"},
		{"node refused",
			"UnhandledPromiseRejectionWarning: Error: connect ECONNREFUSED 127.0.0.1:5432",
			`^UnhandledPromiseRejectionWarning: Error: connect ECONNREFUSED \d+\.\d+\.\d+\.\d+:\d+$`,
			"UnhandledPromiseRejectionWarning: Error: connect ECONNREFUSED 10.0.0.2:6379"},
		{"python traceback",
			"Traceback (most recent call last):",
			`^Traceback \(most recent call last\):$`,
			""},
		{"python missing module",
			"ModuleNotFoundError: No module named 'requests'",
			`^ModuleNotFoundError: No module named '[^']*'$`,
			"ModuleNotFoundError: No module named 'httpx'"},
		{"python frame",
			`  File "/app/main.py", line 12, in <module>`,
			`^\s*File "[^"]*", line \d+, in <module>$`,
			`    File "/app/other.py", line 7, in <module>`},
		{"go panic",
			"panic: runtime error: invalid memory address or nil pointer dereference",
			`^panic: runtime error: invalid memory address or nil pointer dereference$`,
			""},
		{"go build",
			"./main.go:12:5: undefined: foo",
			`^\./main\.go:\d+:\d+: undefined: foo$`,
			"./main.go:40:2: undefined: foo"},
		{"rust error",
			"error[E0425]: cannot find value `x` in this scope",
			"^error\\[E0425\\]: cannot find value `[^`]*` in this scope$",
			"error[E0425]: cannot find value `total` in this scope"},
		{"rust panic",
			"thread 'main' panicked at src/main.rs:4:5:",
			`^thread '[^']*' panicked at src/main\.rs:\d+:\d+:$`,
			"thread 'tokio-runtime-worker' panicked at src/main.rs:9:13:"},
		{"rails template",
			"ActionView::Template::Error (undefined local variable or method `foo' for #<ActionView::Base:0x000000012c3a4b58>):",
			"^ActionView::Template::Error \\(undefined local variable or method `[^']*' for #<ActionView::Base:0[xX][0-9a-fA-F]+>\\):$",
			"ActionView::Template::Error (undefined local variable or method `bar' for #<ActionView::Base:0x00000001deadbeef>):"},
		{"rails nil",
			"NoMethodError (undefined method `name' for nil):",
			"^NoMethodError \\(undefined method `[^']*' for nil\\):$",
			"NoMethodError (undefined method `email' for nil):"},
		{"timestamps and ids",
			"2026-10-26T09:30:00.123Z ERROR request 550e8400-e29b-41d4-a716-446655440000 failed: timeout after 30s",
			`^\d+-\d+-\d+T\d+:\d+:\d+\.\d+Z ERROR request [0-9a-fA-F-]+ failed: timeout after \d+s$`,
			"2026-10-27T11:02:59.001Z ERROR request 123e4567-e89b-12d3-a456-426614174000 failed: timeout after 5s"},
		{"commit and path after a colon",
			"ERROR: commit a1b2c3d4e5 can't be found in /srv/repo",
			`^ERROR: commit [0-9a-fA-F]+ can't be found in \S+$`,
			"ERROR: commit 9f8e7d6c5b can't be found in /srv/repo"},
		{"tab and trailing space",
			"Error: x\twith a tab   ",
			`^Error: x\twith a tab\s*$`,
			"Error: x\twith a tab"},
		{"long line",
			"FATAL lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis nostrud exercitation ullamco laboris",
			`^FATAL lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis`,
			""},
		{"line the engine cut",
			"FATAL boom [1234 more bytes cut]",
			`^FATAL boom`,
			"FATAL boom and the rest of a very long line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := suggestRule(tt.line)
			if got != tt.want {
				t.Errorf("suggestRule(%q)\n got %s\nwant %s", tt.line, got, tt.want)
			}
			if ok, note := checkRule(got, tt.line); !ok {
				t.Errorf("the suggestion fails its own check: %s", note)
			}
			re := regexp.MustCompile(got)
			if tt.repeat != "" && !re.MatchString(tt.repeat) {
				t.Errorf("%s doesn't match the repeat %q", got, tt.repeat)
			}
			for _, h := range healthy {
				if re.MatchString(h) {
					t.Errorf("%s matches healthy output %q", got, h)
				}
			}
		})
	}
}

func TestCheckRule(t *testing.T) {
	const line = "TypeError: Cannot read properties of undefined (reading 'map')"
	tests := []struct {
		expr string
		ok   bool
		want string
	}{
		{`^TypeError: Cannot read`, true, "Compiles and matches"},
		{"", false, "Type a regex"},
		{`(`, false, "Doesn't compile"},
		{`x*`, false, "empty line"},
		{`.`, false, "ordinary output"},
		{`\d`, false, "ordinary output"},
		{`(?i)ok`, false, "ordinary output"},
		{`^ReferenceError`, false, "doesn't match this entry's line"},
		{strings.Repeat("a", maxRule+1), false, "too long"},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			ok, note := checkRule(tt.expr, line)
			if ok != tt.ok || !strings.Contains(note, tt.want) {
				t.Errorf("checkRule(%q) = %v, %q, want %v and %q", tt.expr, ok, note, tt.ok, tt.want)
			}
		})
	}
}
