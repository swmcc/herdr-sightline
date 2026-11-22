package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swmcc/herdr-sightline/internal/config"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "project", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A project file is written by whoever can push to the repo, so anything it
// can't set, or a rule that would match far too much, refuses the whole file.
func TestParseProjectRefuses(t *testing.T) {
	tests := []struct {
		file string
		want []string // each must appear in the error
	}{
		{"settle.toml", []string{"settle.toml:1:18: settle_seconds can't be set from a project file"}},
		{"context.toml", []string{"context_lines can't be set from a project file"}},
		{"pin.toml", []string{"pin.toml:2:3: watch can't be set from a project file"}},
		{"queue.toml", []string{"queue can't be set", "watch can't be set"}},
		{"closing.toml", []string{"closing_line can't be set from a project file"}},
		{"wrong-case.toml", []string{"Settle_Seconds can't be set from a project file"}},
		{"upper-presets.toml", []string{`unknown key "PRESETS" (keys are case sensitive, did you mean presets?)`}},
		{"pattern-agent.toml", []string{"pattern-agent.toml:4:9: agent can't be set in a project file's [[pattern]]"}},
		{"match-everything.toml", []string{`[[pattern]] "everything" matches an empty line`}},
		{"pattern-ordinary.toml", []string{`[[pattern]] "any-e" matches ordinary output like "ready in 312 ms", so it would cry wolf`}},
		{"hide-everything.toml", []string{`[[ignore]] "(?s)." matches ordinary output like " "`}},
		{"hide-errors.toml", []string{`matches common errors like "Error: boom", so it could hide real ones`}},
		{"hide-tracebacks.toml", []string{`matches common errors like "Traceback (most recent call last):"`}},
		{"hide-errors-evasive.toml", []string{"matches common errors like", "so it could hide real ones"}},
		{"hide-go-panics.toml", []string{`matches common errors like "panic: boom"`}},
		{"padded.toml", []string{"has a run of spaces, which could hide part of it on screen"}},
		{"tab.toml", []string{"has a run of spaces"}},
		{"bad-name.toml", []string{"needs a name of at most 64 plain characters"}},
		{"hide-by-length.toml", []string{"[[ignore]]"}},
		{"hide-compose.toml", []string{"[[ignore]]"}},
		{"hide-go-tests.toml", []string{"[[ignore]]"}},
		{"hide-jest.toml", []string{"[[ignore]]"}},
		{"hide-jvm.toml", []string{"[[ignore]]"}},
		{"hide-python.toml", []string{"[[ignore]]"}},
		{"hide-several.toml", []string{"[[ignore]]"}},
		{"hide-turbo.toml", []string{"[[ignore]]"}},
		{"hide-two-words.toml", []string{"[[ignore]]"}},
		{"hide-typescript.toml", []string{"[[ignore]]"}},
		{"hide-webpack.toml", []string{"[[ignore]]"}},
		{"hide-go-index.toml", []string{"[[ignore]]"}},
		{"hide-own-compose.toml", []string{"[[ignore]]"}},
		{"hide-own-turbo.toml", []string{"[[ignore]]"}},
		{"hide-unanchored.toml", []string{"[[ignore]]"}},
		{"bad-regex.toml", []string{"bad regex"}},
		{"syntax.toml", []string{"syntax.toml:"}},
		{"unknown-preset.toml", []string{`unknown preset "cobol"`}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			p, err := config.ParseProject(tt.file, readFixture(t, tt.file))
			if err == nil {
				t.Fatalf("parsed with no error: %+v", p)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q doesn't say %q", err, w)
				}
			}
			if len(p.Patterns)+len(p.Ignores)+len(p.Presets) > 0 || p.PresetsSet {
				t.Errorf("a refused file still gave rules: %+v", p)
			}
		})
	}
}

func TestParseProjectAccepts(t *testing.T) {
	tests := []struct {
		file       string
		presets    []string
		presetsSet bool
		patterns   []string // regexes
		ignores    []string
	}{
		{file: "ok.toml", presets: []string{"go", "node"}, presetsSet: true,
			patterns: []string{`^FATAL \[app\] .+`}, ignores: []string{`^Error: flaky upstream timed out`}},
		{file: "empty.toml"},
		{file: "narrow.toml", ignores: []string{`^Error: flaky upstream timed out`, `^--- FAIL: TestFlakyNetwork `, `^web-1 \| WARN: punycode is deprecated`, `^TypeError: fetch failed \(retrying\)$`}},
		{file: "no-presets.toml", presets: []string{}, presetsSet: true},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			p, err := config.ParseProject(tt.file, readFixture(t, tt.file))
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(p.Presets) != fmt.Sprint(tt.presets) || p.PresetsSet != tt.presetsSet {
				t.Errorf("presets = %v (set %v), want %v (set %v)", p.Presets, p.PresetsSet, tt.presets, tt.presetsSet)
			}
			var patterns, ignores []string
			for _, pat := range p.Patterns {
				patterns = append(patterns, pat.Regex.String())
			}
			for _, ig := range p.Ignores {
				ignores = append(ignores, ig.Regex.String())
			}
			if fmt.Sprint(patterns) != fmt.Sprint(tt.patterns) || fmt.Sprint(ignores) != fmt.Sprint(tt.ignores) {
				t.Errorf("patterns %q ignores %q, want %q and %q", patterns, ignores, tt.patterns, tt.ignores)
			}
		})
	}
}

// The limits are generated rather than kept as fixtures, since a fixture big
// enough to hit them would be mostly noise.
func TestParseProjectLimits(t *testing.T) {
	many := func(table string, n int) string {
		var b strings.Builder
		for i := range n {
			if table == "pattern" {
				fmt.Fprintf(&b, "[[pattern]]\nname = \"p%d\"\nregex = '^FATAL p%d$'\n", i, i)
			} else {
				fmt.Fprintf(&b, "[[ignore]]\nregex = '^WARN: warning number %d$'\n", i)
			}
		}
		return b.String()
	}
	long := "^WARN " + strings.Repeat("x", config.MaxProjectRegex) + "$"
	tests := []struct {
		name string
		data string
		want string // "" for no error
	}{
		{"as many patterns as allowed", many("pattern", config.MaxProjectRules), ""},
		{"one pattern too many", many("pattern", config.MaxProjectRules+1), "at most 50 [[pattern]] tables, this one has 51"},
		{"as many ignores as allowed", many("ignore", config.MaxProjectRules), ""},
		{"one ignore too many", many("ignore", config.MaxProjectRules+1), "at most 50 [[ignore]] tables, this one has 51"},
		{"a regex too long to read", "[[ignore]]\nregex = '" + long + "'\n", "over 500 bytes"},
		{"a file too big", "# " + strings.Repeat("x", config.MaxProjectFileSize) + "\n", "bigger than 64 KiB"},
		// About 30 MB to compile from a few hundred bytes, so it's refused
		// before anything is compiled.
		{"a short regex that's huge to compile", "[[ignore]]\nregex = '^FATAL " + strings.Repeat("[ab]{1000}", 40) + "$'\n", "repeats too much"},
		{"nested repeats", "[[pattern]]\nname = \"x\"\nregex = '^FATAL ((ab|cd){30}){30}'\n", "repeats too much"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.ParseProject("p.toml", []byte(tt.data))
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("error %v, want one saying %q", err, tt.want)
			}
		})
	}
}

// The checks a project rule must pass are the same ones n holds a rule to, so
// every ordinary line is one no accepted rule matches.
func TestParseProjectHoldsRulesToOrdinaryOutput(t *testing.T) {
	for _, line := range config.OrdinaryOutput() {
		re := "^" + strings.ReplaceAll(line, ".", `\.`) + "$"
		for _, table := range []string{"[[pattern]]\nname = \"x\"\n", "[[ignore]]\n"} {
			data := table + "regex = '" + re + "'\n"
			if _, err := config.ParseProject("p.toml", []byte(data)); err == nil {
				t.Errorf("accepted %s matching %q", strings.TrimSpace(table), line)
			}
		}
	}
}
