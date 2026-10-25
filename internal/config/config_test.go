package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertDefaults(t, cfg)
}

func TestParseValid(t *testing.T) {
	tests := []struct {
		fixture string
		check   func(t *testing.T, cfg Config)
	}{
		{"empty.toml", assertDefaults},
		{"full.toml", func(t *testing.T, cfg Config) {
			if cfg.SettleSeconds != 45 {
				t.Errorf("SettleSeconds = %d, want 45", cfg.SettleSeconds)
			}
			if cfg.ContextLines != 10 {
				t.Errorf("ContextLines = %d, want 10", cfg.ContextLines)
			}
			if !slices.Equal(cfg.Presets, []string{"node", "go"}) {
				t.Errorf("Presets = %v, want [node go]", cfg.Presets)
			}
			if len(cfg.Patterns) != 1 || cfg.Patterns[0].Name != "my-framework" ||
				!cfg.Patterns[0].Regex.MatchString("FATAL: it broke") {
				t.Errorf("Patterns = %+v, want my-framework matching FATAL lines", cfg.Patterns)
			}
			if len(cfg.Ignores) != 1 || !cfg.Ignores[0].Regex.MatchString("DeprecationWarning: old") {
				t.Errorf("Ignores = %+v, want one DeprecationWarning rule", cfg.Ignores)
			}
			if !slices.Equal(cfg.Pins, []Pin{{PaneTitle: "web", Agent: "claude"}}) {
				t.Errorf("Pins = %+v, want web to claude", cfg.Pins)
			}
		}},
		{"no-presets.toml", func(t *testing.T, cfg Config) {
			if cfg.Presets == nil || len(cfg.Presets) != 0 {
				t.Errorf("Presets = %#v, want an empty list", cfg.Presets)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			cfg, err := Parse(tt.fixture, readFixture(t, tt.fixture))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

func TestParseErrorsPointAtTheLine(t *testing.T) {
	tests := []struct {
		fixture string
		// Each want is a line of the error, in order.
		want []string
	}{
		{"syntax.toml", []string{"syntax.toml:2:17: "}},
		{"unknown-key.toml", []string{
			`unknown-key.toml:2:1: unknown key "setle_seconds"`,
			`unknown-key.toml:6:1: unknown key "pattern.regx"`,
		}},
		{"bad-regex.toml", []string{"bad-regex.toml:3:9: bad regex (patterns use Go's RE2 syntax): "}},
		{"bad-ignore.toml", []string{"bad-ignore.toml:5:9: bad regex"}},
		{"negative.toml", []string{
			"negative.toml:1:18: settle_seconds must be at least 1, got 0",
			"negative.toml:2:17: context_lines can't be negative, got -1",
		}},
		{"unknown-preset.toml", []string{`unknown-preset.toml:1:20: unknown preset "cobol"`}},
		{"wrong-type.toml", []string{"wrong-type.toml:1:18: settle_seconds has the wrong type (got a TOML string)"}},
		{"missing-fields.toml", []string{
			"missing-fields.toml:1:3: [[pattern]] needs a name",
			"missing-fields.toml:4:3: [[ignore]] needs a regex",
			"missing-fields.toml:6:3: [[watch]] needs an agent",
		}},
		{"duplicate-pattern.toml", []string{`duplicate-pattern.toml:6:8: pattern "same" is defined twice`}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			_, err := Parse(tt.fixture, readFixture(t, tt.fixture))
			if err == nil {
				t.Fatal("Parse succeeded, want an error")
			}
			lines := strings.Split(err.Error(), "\n")
			if len(lines) != len(tt.want) {
				t.Fatalf("got %d errors, want %d:\n%v", len(lines), len(tt.want), err)
			}
			for i, want := range tt.want {
				if !strings.HasPrefix(lines[i], want) {
					t.Errorf("error %d = %q, want it to start %q", i, lines[i], want)
				}
			}
		})
	}
}

func TestLoadReportsTheFilePath(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "context_lines = -5\n")
	_, err := Load(dir)
	if err == nil || !strings.HasPrefix(err.Error(), Path(dir)+":1:") {
		t.Fatalf("Load error = %v, want it to start with the path and line", err)
	}
}

func TestDefaultsAreIndependent(t *testing.T) {
	a := Default()
	a.Presets[0] = "changed"
	if Default().Presets[0] == "changed" || Presets()[0] == "changed" {
		t.Fatal("changing one Config's presets changed the defaults")
	}
}

func assertDefaults(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.SettleSeconds != DefaultSettleSeconds || cfg.ContextLines != DefaultContextLines {
		t.Errorf("got settle %d and context %d, want the defaults", cfg.SettleSeconds, cfg.ContextLines)
	}
	if !slices.Equal(cfg.Presets, Presets()) {
		t.Errorf("Presets = %v, want all of them", cfg.Presets)
	}
	if len(cfg.Patterns)+len(cfg.Ignores)+len(cfg.Pins) != 0 {
		t.Errorf("got patterns, ignores or pins with no file: %+v", cfg)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return data
}

func writeConfig(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(Path(dir), []byte(contents), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
}
