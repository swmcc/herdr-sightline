package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var addedOn = time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC)

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

func writeBytes(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	// WriteFile's mode goes through the umask, so set it outright.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// A regex can hold anything TOML treats specially. Each one has to load back
// as exactly what was typed.
func TestAppendIgnoreRoundTrips(t *testing.T) {
	tests := []struct {
		name  string
		regex string
		want  string // the value as written, to show which kind of string was picked
	}{
		{"plain", `DeprecationWarning`, `'DeprecationWarning'`},
		{"backslashes stay readable", `^Error: \d+\.\d+ \(x\)`, `'^Error: \d+\.\d+ \(x\)'`},
		{"double quotes", `reading "map"`, `'reading "map"'`},
		{"single quote", `reading '[^']*'`, `"reading '[^']*'"`},
		{"single quote and backslash", `can't \d+`, `"can't \\d+"`},
		{"three single quotes", `'''`, `"'''"`},
		{"three double quotes", `"""`, `'"""'`},
		{"both quotes", `'"\\`, `"'\"\\\\"`},
		{"tab", "a\tb", "'a\tb'"},
		{"control character", "a\x01b", `"a\u0001b"`},
		{"delete", "a\x7fb", `"a\u007Fb"`},
		{"backslash at the end", `C:\\`, `'C:\\'`},
		{"unicode", `✗ échec`, `'✗ échec'`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			a, err := AppendIgnore(dir, tt.regex, addedOn)
			if err != nil {
				t.Fatalf("AppendIgnore: %v", err)
			}
			if !strings.HasSuffix(a.Block, "regex = "+tt.want+"\n") {
				t.Errorf("block = %q, want the value written as %s", a.Block, tt.want)
			}
			cfg, err := Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Ignores) != 1 || cfg.Ignores[0].Regex.String() != tt.regex {
				t.Errorf("loaded %v, want exactly %q", cfg.Ignores, tt.regex)
			}
		})
	}
}

func TestAppendIgnoreRefusesBadRules(t *testing.T) {
	tests := []struct {
		name  string
		regex string
		want  string
	}{
		{"empty", "", "needs a regex"},
		{"newline", "a\nb", "one line"},
		{"carriage return", "a\rb", "one line"},
		{"not UTF-8", "a\xffb", "UTF-8"},
		{"doesn't compile", "(", "bad regex"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := AppendIgnore(dir, tt.regex, addedOn)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
			if _, err := os.Stat(Path(dir)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused rule still wrote the file (stat: %v)", err)
			}
		})
	}
}

// Everything already in the file stays byte for byte, and the new table goes
// on the end after one blank line.
func TestAppendIgnoreKeepsTheFile(t *testing.T) {
	fixture := readBytes(t, filepath.Join("testdata", "append-existing.toml"))
	const block = "# added from the feed on 2026-10-26\n[[ignore]]\nregex = '^TypeError: x'\n"
	tests := []struct {
		name     string
		existing []byte
		sep      string
	}{
		{"ends with a newline", fixture, "\n"},
		{"no newline at the end", []byte(strings.TrimSuffix(string(fixture), "\n")), "\n\n"},
		{"windows line endings", []byte(strings.ReplaceAll(string(fixture), "\n", "\r\n")), "\n"},
		{"empty", nil, ""},
		{"byte order mark", append([]byte("\xEF\xBB\xBF"), fixture...), "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := Path(dir)
			writeBytes(t, path, tt.existing, 0o640)

			a, err := AppendIgnore(dir, `^TypeError: x`, addedOn)
			if err != nil {
				t.Fatalf("AppendIgnore: %v", err)
			}
			got := readBytes(t, path)
			if want := string(tt.existing) + tt.sep + block; string(got) != want {
				t.Errorf("file is\n%q\nwant\n%q", got, want)
			}
			if a.Block != tt.sep+block {
				t.Errorf("Block = %q, want %q", a.Block, tt.sep+block)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o640 {
				t.Errorf("mode = %v, want it kept at 0640", info.Mode().Perm())
			}
			cfg, err := Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if last := cfg.Ignores[len(cfg.Ignores)-1].Regex.String(); last != `^TypeError: x` {
				t.Errorf("last ignore = %q", last)
			}

			// Undo puts it back exactly as it was.
			if err := RemoveAppended(a); err != nil {
				t.Fatalf("RemoveAppended: %v", err)
			}
			if got := readBytes(t, path); string(got) != string(tt.existing) {
				t.Errorf("after undo the file is\n%q\nwant\n%q", got, tt.existing)
			}
		})
	}
}

func TestAppendIgnoreCreatesTheFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugins", "swmcc.sightline")
	if _, err := AppendIgnore(dir, `x`, addedOn); err != nil {
		t.Fatalf("AppendIgnore: %v", err)
	}
	got := readBytes(t, Path(dir))
	if want := "# added from the feed on 2026-10-26\n[[ignore]]\nregex = 'x'\n"; string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	if len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// There was no file before, so undo leaves none.
func TestRemoveAppendedRemovesAFileItCreated(t *testing.T) {
	dir := t.TempDir()
	a, err := AppendIgnore(dir, `x`, addedOn)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Created {
		t.Error("Created isn't set for a new file")
	}
	if err := RemoveAppended(a); err != nil {
		t.Fatalf("RemoveAppended: %v", err)
	}
	if _, err := os.Stat(Path(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file is still there after undo (stat: %v)", err)
	}
}

// The feed must never break the config or write somewhere odd.
func TestAppendIgnoreRefusesToRiskTheConfig(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{"directory", func(t *testing.T, dir string) {
			if err := os.Mkdir(Path(dir), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		{"symlink to nothing", func(t *testing.T, dir string) {
			if err := os.Symlink(filepath.Join(dir, "missing.toml"), Path(dir)); err != nil {
				t.Skipf("can't make a symlink here: %v", err)
			}
		}, "symlink to a file that doesn't exist"},
		{"already broken", func(t *testing.T, dir string) {
			writeBytes(t, Path(dir), []byte("settle_seconds = \n"), 0o600)
		}, "already has a problem"},
		{"unclosed string", func(t *testing.T, dir string) {
			writeBytes(t, Path(dir), []byte("[[pattern]]\nname = \"x\"\nregex = '''\n"), 0o600)
		}, "already has a problem"},
		{"too big", func(t *testing.T, dir string) {
			// Just under the cap, so the file loads but the rule won't fit.
			writeBytes(t, Path(dir), []byte("#"+strings.Repeat("x", MaxFileSize-20)+"\n"), 0o600)
		}, "over 1 MiB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)
			var before []byte
			if info, err := os.Stat(Path(dir)); err == nil && info.Mode().IsRegular() {
				before = readBytes(t, Path(dir))
			}
			_, err := AppendIgnore(dir, `x`, addedOn)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
			if before != nil && string(readBytes(t, Path(dir))) != string(before) {
				t.Error("the file changed even though the rule was refused")
			}
		})
	}
}

// A config kept in a dotfiles repo and linked in stays linked.
func TestAppendIgnoreFollowsASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "sightline.toml")
	writeBytes(t, target, []byte("settle_seconds = 45\n"), 0o644)
	if err := os.Symlink(target, Path(dir)); err != nil {
		t.Skipf("can't make a symlink here: %v", err)
	}
	a, err := AppendIgnore(dir, `x`, addedOn)
	if err != nil {
		t.Fatalf("AppendIgnore: %v", err)
	}
	info, err := os.Lstat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a file")
	}
	if !strings.HasSuffix(string(readBytes(t, target)), "regex = 'x'\n") {
		t.Error("the file the link points at didn't get the rule")
	}
	if err := RemoveAppended(a); err != nil {
		t.Fatalf("RemoveAppended: %v", err)
	}
	if got := string(readBytes(t, target)); got != "settle_seconds = 45\n" {
		t.Errorf("after undo the target is %q", got)
	}
}

func TestRemoveAppended(t *testing.T) {
	const original = "settle_seconds = 45\n"
	tests := []struct {
		name    string
		change  func(s string) string // what happens to the file after the append
		want    string                // the file after undo, with %s for original
		wantErr error
	}{
		{"untouched", func(s string) string { return s }, original, nil},
		{"edited above the rule", func(s string) string {
			return strings.Replace(s, "45", "60", 1)
		}, "settle_seconds = 60\n", nil},
		{"something added after it", func(s string) string {
			return s + "# mine\n"
		}, "", ErrChanged},
		{"the rule edited", func(s string) string {
			return strings.Replace(s, "regex = 'x'", "regex = 'y'", 1)
		}, "", ErrChanged},
		{"deleted", func(string) string { return "" }, "", ErrChanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBytes(t, Path(dir), []byte(original), 0o600)
			a, err := AppendIgnore(dir, `x`, addedOn)
			if err != nil {
				t.Fatal(err)
			}
			changed := tt.change(string(readBytes(t, Path(dir))))
			writeBytes(t, Path(dir), []byte(changed), 0o600)

			err = RemoveAppended(a)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			got := string(readBytes(t, Path(dir)))
			if tt.wantErr != nil {
				if got != changed {
					t.Errorf("a refused undo changed the file to %q", got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("after undo the file is %q, want %q", got, tt.want)
			}
		})
	}
}
