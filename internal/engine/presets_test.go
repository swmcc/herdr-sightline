package engine

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const patternsDir = "testdata/patterns"

// fixtureLines returns the non-blank lines of a fixture, exactly as written.
func fixtureLines(t testing.TB, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("%s has no lines, so it tests nothing", path)
	}
	return lines
}

// TestUnlessNamesAPattern catches an exception left behind by a renamed pattern.
func TestUnlessNamesAPattern(t *testing.T) {
	names := map[string]bool{}
	for _, m := range allMatchers(t) {
		names[m.Name()] = true
	}
	for name := range unless {
		if !names[name] {
			t.Errorf("unless has %q, which isn't a pattern", name)
		}
	}
}

// allMatchers is every built-in matcher, in the engine's order.
func allMatchers(t testing.TB) []Matcher {
	t.Helper()
	ms, err := Presets(PresetNames()...)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// matchedBy returns the names of every matcher that matches line.
func matchedBy(ms []Matcher, line string) []string {
	var names []string
	for _, m := range ms {
		if m.Match(line) {
			names = append(names, m.Name())
		}
	}
	return names
}

// TestPatternFixtures checks every pattern against its own fixtures. Each match
// line must be caught by the pattern. Each no-match line must be caught by no
// pattern in any preset, because a healthy line from one tool can turn up in a
// pane where every preset is on.
func TestPatternFixtures(t *testing.T) {
	all := allMatchers(t)
	for _, p := range presets {
		for i, pat := range p.patterns {
			m := compiled[p.name][i]
			base := filepath.Join(patternsDir, p.name, pat.name)
			t.Run(m.Name(), func(t *testing.T) {
				for _, line := range fixtureLines(t, base+".match.txt") {
					if !m.Match(line) {
						t.Errorf("%s missed a real error:\n  %q", m.Name(), line)
					}
				}
				for _, line := range fixtureLines(t, base+".nomatch.txt") {
					if got := matchedBy(all, line); len(got) > 0 {
						t.Errorf("%v matched healthy output from %s:\n  %q", got, base+".nomatch.txt", line)
					}
				}
			})
		}
	}
}

// TestNoStrayFixtures catches a fixture whose pattern was renamed or removed, so
// it can't sit there quietly testing nothing.
func TestNoStrayFixtures(t *testing.T) {
	known := map[string]bool{}
	for _, p := range presets {
		for _, pat := range p.patterns {
			known[filepath.Join(patternsDir, p.name, pat.name)] = true
		}
	}
	files, err := filepath.Glob(filepath.Join(patternsDir, "*", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		switch {
		case strings.HasSuffix(f, ".trace.txt"):
		case strings.HasSuffix(f, ".nomatch.txt"):
			if !known[strings.TrimSuffix(f, ".nomatch.txt")] {
				t.Errorf("%s has no pattern", f)
			}
		case strings.HasSuffix(f, ".match.txt"):
			if !known[strings.TrimSuffix(f, ".match.txt")] {
				t.Errorf("%s has no pattern", f)
			}
		default:
			t.Errorf("%s is not a .match, .nomatch or .trace fixture", f)
		}
	}
}

// TestPrefixedFixtures runs every fixture again as if it came through docker
// compose, foreman or turborepo, which put a label in front of each line.
func TestPrefixedFixtures(t *testing.T) {
	prefixes := []string{"web-1  | ", "12:00:00 web.1  | ", "@acme/web:dev: "}
	labelled := regexp.MustCompile(labelPrefix)
	all := allMatchers(t)
	for _, p := range presets {
		for i, pat := range p.patterns {
			m := compiled[p.name][i]
			base := filepath.Join(patternsDir, p.name, pat.name)
			t.Run(m.Name(), func(t *testing.T) {
				for _, line := range fixtureLines(t, base+".match.txt") {
					if labelled.FindString(line) != "" {
						continue // already labelled, and a label is only allowed once
					}
					for _, pre := range prefixes {
						if !m.Match(pre + line) {
							t.Errorf("%s missed a labelled error:\n  %q", m.Name(), pre+line)
						}
					}
				}
				for _, line := range fixtureLines(t, base+".nomatch.txt") {
					for _, pre := range prefixes {
						if got := matchedBy(all, pre+line); len(got) > 0 {
							t.Errorf("%v matched labelled healthy output:\n  %q", got, pre+line)
						}
					}
				}
			})
		}
	}
}

// TestContinuationLines checks that the lines in the middle of a trace never
// start an error of their own. The capture window collects them instead.
func TestContinuationLines(t *testing.T) {
	all := allMatchers(t)
	tests := []struct {
		name string
		line string
	}{
		{"node frame", "    at Object.<anonymous> (/app/src/server.js:12:11)"},
		{"node async frame", "    at async Promise.all (index 0)"},
		{"node cause", "  [cause]: Error: connect ECONNREFUSED 127.0.0.1:5432"},
		{"node header", "/app/src/server.js:12"},
		{"python frame", `  File "/app/views.py", line 18, in detail`},
		{"python source", "    return users[pk]"},
		{"python chain", "During handling of the above exception, another exception occurred:"},
		{"ruby frame", "\tfrom app.rb:7:in '<main>'"},
		{"rails frame", "app/controllers/posts_controller.rb:6:in 'PostsController#show'"},
		{"go goroutine", "goroutine 1 [running]:"},
		{"go frame", "\t/app/main.go:12 +0x1d"},
		{"go func", "main.(*Server).Start(0x0)"},
		{"go subtest", "    --- FAIL: TestParse/empty (0.00s)"},
		{"go test log", "    parse_test.go:21: Parse(\"\") error = <nil>, want an error"},
		{"rust location", " --> src/main.rs:8:5"},
		{"rust backtrace", "   0: rust_begin_unwind"},
		{"java frame", "\tat com.acme.App.main(App.java:12)"},
		{"java more", "\t... 23 more"},
		{"java cause", "Caused by: java.io.FileNotFoundException: config.yml (No such file or directory)"},
		{"jest detail", "  ● formatDate › handles midnight"},
		{"rspec detail", "     Failure/Error: expect(user.full_name).to eq(\"Ada Lovelace\")"},
		{"pytest detail", "E       assert 200 == 404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchedBy(all, tt.line); len(got) > 0 {
				t.Errorf("%v started an error on a continuation line:\n  %q", got, tt.line)
			}
		})
	}
}

// TestTraces runs whole pieces of real output through the engine with every
// preset on and counts the lines that start an error. Each trace says how many
// it expects on its first line, as "# starts: N".
func TestTraces(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(patternsDir, "*", "*.trace.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no traces found")
	}
	e := New(Options{Matchers: allMatchers(t)})
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			want, lines := readTrace(t, f)
			var started []string
			for _, line := range lines {
				if name, ok := e.startsError(line); ok {
					started = append(started, fmt.Sprintf("%s: %q", name, line))
				}
			}
			if len(started) != want {
				t.Errorf("%d lines started an error, want %d:\n  %s", len(started), want, strings.Join(started, "\n  "))
			}
		})
	}
}

func readTrace(t *testing.T, path string) (starts int, lines []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatalf("%s is empty", path)
	}
	header, ok := strings.CutPrefix(sc.Text(), "# starts: ")
	if !ok {
		t.Fatalf("%s must start with \"# starts: N\"", path)
	}
	n, _, _ := strings.Cut(header, " ")
	starts, err = strconv.Atoi(n)
	if err != nil {
		t.Fatalf("%s: bad header: %v", path, err)
	}
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return starts, lines
}

func TestPresets(t *testing.T) {
	names := func(ms []Matcher) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Name())
		}
		return out
	}
	t.Run("every preset has patterns", func(t *testing.T) {
		for _, n := range PresetNames() {
			ms, err := Presets(n)
			if err != nil || len(ms) == 0 {
				t.Errorf("Presets(%q) = %d matchers, %v", n, len(ms), err)
			}
		}
	})
	t.Run("unknown name", func(t *testing.T) {
		if _, err := Presets("go", "cobol"); err == nil || !strings.Contains(err.Error(), `"cobol"`) {
			t.Fatalf("err = %v, want one naming cobol", err)
		}
	})
	t.Run("none", func(t *testing.T) {
		ms, err := Presets()
		if err != nil || len(ms) != 0 {
			t.Fatalf("Presets() = %v, %v, want nothing", ms, err)
		}
	})
	t.Run("engine order and no repeats", func(t *testing.T) {
		ms, err := Presets("generic", "go", "generic")
		if err != nil {
			t.Fatal(err)
		}
		got := names(ms)
		goMs, _ := Presets("go")
		genericMs, _ := Presets("generic")
		want := append(names(goMs), names(genericMs)...)
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("generic goes last", func(t *testing.T) {
		got := PresetNames()
		if got[len(got)-1] != "generic" {
			t.Fatalf("PresetNames() = %v, want generic last", got)
		}
	})
	t.Run("names are preset slash pattern", func(t *testing.T) {
		seen := map[string]bool{}
		for _, n := range names(allMatchers(t)) {
			if seen[n] {
				t.Errorf("%s appears twice", n)
			}
			seen[n] = true
			if p, _, ok := strings.Cut(n, "/"); !ok || !slices.Contains(PresetNames(), p) {
				t.Errorf("%s isn't named after its preset", n)
			}
		}
	})
}

func TestRegexp(t *testing.T) {
	tests := []struct {
		name string
		m    Matcher
		line string
		want bool
	}{
		{"matches", Regexp("my-framework", regexp.MustCompile(`^FATAL: .+`)), "FATAL: disk full", true},
		{"misses", Regexp("my-framework", regexp.MustCompile(`^FATAL: .+`)), "all good", false},
		{"nil never matches", Regexp("broken", nil), "anything", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.Match(tt.line); got != tt.want {
				t.Fatalf("Match(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
	if got := Regexp("my-framework", nil).Name(); got != "my-framework" {
		t.Fatalf("Name() = %q", got)
	}
}

// TestIgnoreBeatsPreset is the [[ignore]] path: a user's rule silences a line a
// preset would have caught.
func TestIgnoreBeatsPreset(t *testing.T) {
	e := New(Options{
		Matchers: allMatchers(t),
		Ignores:  []Matcher{Regexp("", regexp.MustCompile(`EADDRINUSE`))},
	})
	if name, ok := e.startsError("Error: listen EADDRINUSE: address already in use :::3000"); ok {
		t.Fatalf("ignored line started an error from %s", name)
	}
	if name, ok := e.startsError("TypeError: x is not a function"); !ok || name != "node/exception" {
		t.Fatalf("startsError = %q, %v, want node/exception", name, ok)
	}
}

// TestCompiledOnce shows the patterns are compiled once at start up and shared,
// not compiled per call or per line. BenchmarkPresets shows the matching itself
// doesn't allocate. That can't be a test, because -race makes sync.Pool drop
// regexp's scratch space at random.
func TestCompiledOnce(t *testing.T) {
	first, second := allMatchers(t), allMatchers(t)
	for i := range first {
		a, aok := first[i].(presetMatcher)
		b, bok := second[i].(presetMatcher)
		if !aok || !bok || a.re == nil || a.re != b.re {
			t.Fatalf("%s isn't one shared compiled pattern", first[i].Name())
		}
	}
}

// BenchmarkPresets matches a few thousand lines of mixed output against every
// preset, the way a busy dev server would.
func BenchmarkPresets(b *testing.B) {
	files, err := filepath.Glob(filepath.Join(patternsDir, "*", "*.txt"))
	if err != nil {
		b.Fatal(err)
	}
	var corpus []string
	for _, f := range files {
		corpus = append(corpus, fixtureLines(b, f)...)
	}
	for len(corpus) < 5000 {
		corpus = append(corpus, corpus...)
	}
	e := New(Options{Matchers: allMatchers(b)})
	for b.Loop() {
		for _, line := range corpus {
			e.startsError(line)
		}
	}
	b.ReportMetric(float64(len(corpus)), "lines/op")
}
