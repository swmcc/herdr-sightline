package feed

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
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
		{"padded with non-breaking spaces",
			" Error: boom ",
			`^\x{a0}Error: boom\x{a0}$`,
			""},
		{"padded with vertical tabs",
			"\vError: boom\v",
			`^\x{b}Error: boom\x{b}$`,
			""},
		{"carriage return in a path",
			"error: at foo/bar\rbaz",
			`^error: at \S+\x{d}baz$`,
			""},
		{"unclosed quote",
			"Error: can't open 'config.yml",
			`^Error: can't open 'config\.yml$`,
			""},
		{"bytes that aren't text",
			"Error: bad \xff byte\x07 here",
			`^Error: bad \x{fffd} byte\x{7} here$`,
			""},
		{"long line",
			"FATAL lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis nostrud exercitation ullamco laboris",
			`^FATAL lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis`,
			""},
		{"line the engine cut",
			"FATAL boom " + strings.Repeat("x ", 100) + "[1234 more bytes cut]",
			`^FATAL boom ` + strings.TrimSpace(strings.Repeat("x ", 74)),
			"FATAL boom " + strings.Repeat("x ", 100) + "and the rest of a very long line"},
		{"short line with a cut marker it printed itself",
			"FATAL boom [1234 more bytes cut]",
			`^FATAL boom \[\d+ more bytes cut\]$`,
			""},
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

// The pane chooses the line, so it can try to steer the suggestion into a rule
// that hides a whole kind of error. Whatever is suggested must still pass the
// check and must not match the other error.
func TestSuggestRuleResistsHostileLines(t *testing.T) {
	long := strings.Repeat("A", 200)
	tests := []struct {
		name   string
		line   string
		victim string // another error the rule must not hide
	}{
		{"fake cut marker", "Error: [1 more bytes cut]", "Error: connect ECONNREFUSED 127.0.0.1:5432"},
		{"fake cut marker after panic", "panic: [9 more bytes cut]", "panic: runtime error: index out of range"},
		{"early space then a long word", "panic: " + long, "panic: runtime error: index out of range"},
		{"early space after Error", "Error: " + long, "Error: boom"},
		{"next marker only", " ⨯ ./" + long, " ⨯ Error: Failed to collect page data"},
		{"go test failure", "--- FAIL: " + long, "--- FAIL: TestOther (0.00s)"},
		{"prefixed", "web | Error: " + long, "web | Error: something else"},
		{"path collapses to any word", "Error: /x", "Error: boom"},
		{"several paths", "Error: /x /y /z", "Error: cannot read property"},
		{"quote collapses to anything", "Error: 'x'", "Error: 'anything at all'"},
		{"python key", "KeyError: 'user_id'", "KeyError: 'email'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := suggestRule(tt.line)
			if ok, note := checkRule(got, tt.line); !ok {
				t.Errorf("suggestion %s fails its own check: %s", got, note)
			}
			if regexp.MustCompile(got).MatchString(tt.victim) {
				t.Errorf("suggestion %s for %q also hides %q", got, tt.line, tt.victim)
			}
			// Typing the broad rule by hand is refused too.
			if ok, _ := checkRule(`^`+regexp.QuoteMeta(strings.SplitAfter(tt.line, ": ")[0]), tt.line); ok && strings.Contains(tt.line, ": ") {
				t.Errorf("checkRule let the bare prefix of %q through", tt.line)
			}
		})
	}
}

// Every character of the rule is drawn, however wide, so what's saved is what
// was on screen.
func TestRuleLinesFitTheWidth(t *testing.T) {
	text := []rune("Error: " + strings.Repeat("错", 30) + " SECRET " + strings.Repeat("误", 30))
	const w = 40
	var drawn strings.Builder
	for _, l := range ruleLines(text, -1, w) {
		if got := runewidth.StringWidth(l); got > w {
			t.Errorf("row is %d cells, wider than %d: %q", got, w, l)
		}
		drawn.WriteString(strings.TrimPrefix(l, "  "))
	}
	if got := strings.TrimSuffix(drawn.String(), " "); got != string(text) {
		t.Errorf("drawn %q, want all of %q", got, string(text))
	}
}

// Whatever a pane prints, the suggestion compiles, is text and matches the line.
func FuzzSuggestRule(f *testing.F) {
	for _, s := range []string{
		"TypeError: Cannot read properties of undefined (reading 'map')",
		" Error: boom\v",
		"Error: bad \xff byte",
		"error: at foo/bar\rbaz",
		"FATAL boom [12 more bytes cut]",
		"Error: " + strings.Repeat("错", 100),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		got := suggestRule(line)
		if !utf8.ValidString(got) {
			t.Fatalf("suggestion %q isn't valid UTF-8", got)
		}
		re, err := regexp.Compile(got)
		if err != nil {
			t.Fatalf("suggestion %q doesn't compile: %v", got, err)
		}
		if !re.MatchString(line) {
			t.Fatalf("suggestion %q doesn't match %q", got, line)
		}
	})
}
