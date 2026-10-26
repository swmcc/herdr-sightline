package engine

import (
	"fmt"
	"regexp"
)

// The built-in patterns. Each one matches the line that starts an error, and the
// capture window collects the rest of the trace. They're written to miss an odd
// error rather than cry wolf: every pattern is anchored at the start of the line
// and matches the shape of an error, not just a word.
//
// Every pattern has fixtures in testdata/patterns/<preset>/<name>.match.txt and
// <name>.nomatch.txt. See .claude/workflows/add-pattern.md before changing one.
//
// Anchoring has one known weak spot. An unterminated escape at the end of one
// line can eat the first character of the next, as a terminal would, and then an
// anchored pattern misses it. The scrubber flags that as a possible gap, so the
// miss is never silent.

// labelPrefix allows for the labels process managers put in front of each line:
// docker compose ("web-1  | "), foreman and overmind ("12:00:00 web.1  | ") and
// turborepo ("web:dev: "). Without it, nothing run under one of them would match.
//
// A compose or foreman label starts lower case, so a log level column such as
// "WARN | " isn't taken for one. A turborepo task name starts with a letter, so
// "main.go:42: " from log.Lshortfile isn't either. Labels from concurrently
// ("[web] ") and pnpm ("apps/web dev: ") aren't allowed yet: "[tsl] ERROR in"
// from ts-loader would then start a second error under webpack's.
const labelPrefix = `^(?:(?:\d{2}:\d{2}:\d{2} )?[a-z0-9][\w.-]* *\| |[\w@./-]+:[A-Za-z][\w-]*: )?`

// emptyValue is a status line such as "error: none" or "Fatal: 0", not an error.
const emptyValue = `(?:0|none|None|null|nil|false|ok|OK)\s*$`

type pattern struct {
	name string
	expr string // RE2, matched after labelPrefix
}

// unless holds, for some patterns, lines that look like a match but never are.
// They're matched after labelPrefix too, and cover shapes RE2 can't rule out
// without lookahead.
var unless = map[string]string{
	"node/exception": `(?:Uncaught )?[A-Za-z]*Error: ` + emptyValue,
	"generic/error":  `(?:error|Error|ERROR): ` + emptyValue,
	"generic/fatal":  `(?:fatal|Fatal|FATAL): +` + emptyValue,
}

// presetMatcher is a built-in pattern with its optional exception.
type presetMatcher struct {
	name   string
	re     *regexp.Regexp
	unless *regexp.Regexp
}

func (m presetMatcher) Name() string { return m.name }

func (m presetMatcher) Match(line string) bool {
	return m.re.MatchString(line) && (m.unless == nil || !m.unless.MatchString(line))
}

type preset struct {
	name     string
	patterns []pattern
}

// presets is in the order matchers are tried, which decides the name shown when
// two patterns match the same line. generic goes last so the more specific name
// wins. The names must match config.Presets.
var presets = []preset{
	{"node", []pattern{
		// The first line of err.stack for JavaScript's own error types. Custom
		// classes (AxiosError and friends) are left out because their names look
		// just like the last line of a Python traceback, and so is AssertionError.
		// TypeError and SyntaxError still overlap with Python. That's accepted:
		// they're the most common Node crashes of all.
		{"exception", `(?:Uncaught )?(?:Error|TypeError|RangeError|ReferenceError|SyntaxError|EvalError|URIError|AggregateError)(?: \[[A-Z][A-Z0-9_]*\])?: \S`},
		{"unhandled-rejection", `\[UnhandledPromiseRejection: \S`},
		// Next.js marks errors with ⨯. Its "app/page.tsx (12:18) @ Page" line can
		// sit next to the error itself, so only the error line is matched.
		{"next", ` *⨯ (?:\[?(?:Uncaught )?(?:[A-Z]\w*)?Error\b|unhandledRejection: |uncaughtException: |Failed to |\./\S)`},
		{"typescript", `(?:[^\s(]+(?:\(\d+,\d+\):|:\d+:\d+ -) )?error TS\d+: \S`},
		{"webpack", `ERROR in \S`},
		{"vite", `(?:\d{1,2}:\d{2}:\d{2}(?: [AP]M)? )?(?:\[vite\] (?:Internal server error|Pre-transform error|error while updating dependencies)|error during build:$)`},
		{"esbuild", `[✘X] \[ERROR\] \S`},
		// "npm error code 1" follows the crash of whatever npm ran, which is
		// already caught, so it's left out. A missing script has nothing above it.
		{"npm", `npm (?:ERR!|error) (?:Missing|missing) script:`},
	}},
	{"python", []pattern{
		{"traceback", `(?:  \+ )?(?:Exception Group )?Traceback \(most recent call last\):$`},
	}},
	{"ruby", []pattern{
		// app.rb:3:in 'divide': divided by 0 (ZeroDivisionError)
		{"exception", "[^\\s:]+:\\d+:in [`'][^']*': .* \\([A-Z]\\w*(?:::[A-Z]\\w*)*\\)$"},
		// Only 5xx. A RoutingError for /favicon.ico is noise, not a bug.
		{"rails-500", `(?:\[[^\]]+\] )*Completed 5\d\d \S`},
		{"aborted", `(?:bin/)?(?:rails|rake) aborted!$`},
	}},
	{"go", []pattern{
		{"panic", `panic: \S`},
		{"fatal", `fatal error: \S`},
		// go build, go run, go vet and go test print "# <package>" above a
		// package's errors. Matching that, not each file:line:col line under it,
		// makes one failed build one error. It needs a slash in the path (or
		// command-line-arguments) so a "# heading" in other output can't match.
		{"build", `# (?:command-line-arguments|[\w.-]+(?:/[\w.@~-]+)+)(?: \[[\w.@~/-]+\.test\])?$`},
		{"race", `WARNING: DATA RACE$`},
	}},
	{"rust", []pattern{
		{"panic", `thread '[^']*'(?: \(\d+\))? panicked at \S`},
		{"compile", `error\[E\d{4}\]: \S`},
	}},
	{"jvm", []pattern{
		{"thread", `Exception in thread "[^"]*" \S`},
		// e.printStackTrace() and Spring print the exception on its own line. The
		// package has to start with a usual JVM root, because the last line of a
		// Python traceback (sqlalchemy.exc.OperationalError: ...) looks the same
		// and would start a second error inside the traceback.
		{"exception", `(?:java|javax|jakarta|kotlin|kotlinx|scala|groovy|clojure|org|com|io|net|dev)\.(?:[a-z][\w$]*\.)*[A-Z][\w$]*(?:Exception|Error)(?:: .*)?$`},
		{"spring", `APPLICATION FAILED TO START$`},
		{"gradle", `FAILURE: Build (?:failed with an exception\.|completed with \d+ failures?\.)$`},
		{"maven", `\[(?:INFO|ERROR)\] BUILD FAILURE$`},
	}},
	{"tests", []pattern{
		{"jest", ` *FAIL +[^\s:]+\.[cm]?[jt]sx?(?: |$)`},
		{"mocha", ` +[1-9]\d* failing$`},
		{"pytest", `=+ (?:FAILURES|ERRORS) =+$`},
		{"unittest", `(?:FAIL|ERROR): \w+ \(`},
		{"rspec", `(?:Failures:$|An error occurred (?:while loading \S|in an? \S))`},
		// Rails prints "Failure:" on its own line, plain minitest "  1) Failure:".
		{"minitest", `(?: +\d+\) )?(?:Failure|Error):$`},
		{"go-test", `--- FAIL: \S`},
		{"cargo-test", `test result: FAILED\.`},
	}},
	{"generic", []pattern{
		// One space after the colon. uvicorn pads its level ("ERROR:    ") and
		// prints a traceback straight after, which python/traceback catches.
		{"error", `(?:error|Error|ERROR)(?:\[[\w-]+\])?: \S`},
		{"fatal", `(?:PHP )?(?:fatal|Fatal|FATAL)(?: (?:error|Error|ERROR))?: +\S`},
		// gcc, clang, swiftc and friends give a column: main.c:3:5: error: ...
		// javac doesn't. Other files without one are left alone, because that's
		// also what log.Lshortfile prints in front of a logged error.
		{"compiler", `(?:[^\s:]+:\d+:\d+: (?:fatal )?error: \S|[^\s:]+\.java:\d+: error: \S)`},
		{"crash", `(?:(?:zsh|bash): )?(?:[Ss]egmentation fault|[Bb]us error)(?:: \d+)?(?: \(core dumped\))?(?: {2,}\S.*)?$`},
		{"not-found", `(?:zsh: command not found: \S+|(?:bash|sh): (?:line \d+: )?\S+: command not found|sh: \d+: \S+: not found)$`},
	}},
}

// compiled holds every preset's matchers, built once. A bad built-in pattern is a
// bug in Sightline, so it panics at start up rather than at the first line.
var compiled = compilePresets()

func compilePresets() map[string][]Matcher {
	out := make(map[string][]Matcher, len(presets))
	for _, p := range presets {
		ms := make([]Matcher, 0, len(p.patterns))
		for _, pat := range p.patterns {
			m := presetMatcher{name: p.name + "/" + pat.name, re: regexp.MustCompile(labelPrefix + pat.expr)}
			if u, ok := unless[m.name]; ok {
				m.unless = regexp.MustCompile(labelPrefix + u)
			}
			ms = append(ms, m)
		}
		out[p.name] = ms
	}
	return out
}

// PresetNames returns the name of every built-in preset, in the order they're
// tried.
func PresetNames() []string {
	names := make([]string, len(presets))
	for i, p := range presets {
		names[i] = p.name
	}
	return names
}

// Presets returns the matchers for the named presets. They come back in the
// engine's own order, not the order asked for, so the most specific name is the
// one shown. Asking twice for a preset is harmless. An unknown name is an error.
func Presets(names ...string) ([]Matcher, error) {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		if _, ok := compiled[n]; !ok {
			return nil, fmt.Errorf("unknown preset %q", n)
		}
		want[n] = true
	}
	var out []Matcher
	for _, p := range presets {
		if want[p.name] {
			out = append(out, compiled[p.name]...)
		}
	}
	return out, nil
}
