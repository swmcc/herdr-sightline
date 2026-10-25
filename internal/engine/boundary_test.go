package engine_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// engineRoot is the only import path outside the standard library the engine and
// its own packages may depend on.
const engineRoot = "github.com/swmcc/herdr-sightline/internal/engine"

// TestBoundary fails if the engine, its subpackages or their tests depend on
// anything but the standard library and the engine itself. That rules out the
// herdr adapter, the config package and every third party module. See #3.
func TestBoundary(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go tool isn't on PATH, so the boundary can't be checked")
	}
	// The test runs in the package directory, so ./... is the engine and its subpackages.
	// Only stdout is parsed, so toolchain or download chatter on stderr can't
	// pass for a dependency.
	out, err := exec.Command(goTool, "list", "-deps", "-test",
		"-f", "{{.ImportPath}}\t{{.Standard}}", "./...").Output()
	if err != nil {
		var stderr []byte
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = exitErr.Stderr
		}
		t.Fatalf("go list: %v\n%s", err, stderr)
	}
	if bad := outsideBoundary(strings.Split(strings.TrimSpace(string(out)), "\n")); len(bad) > 0 {
		t.Fatalf("the engine must use only the standard library, but depends on:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

func TestOutsideBoundary(t *testing.T) {
	tests := []struct {
		name string
		line string
		bad  bool
	}{
		{"standard library", "strings\ttrue", false},
		{"the engine", engineRoot + "\tfalse", false},
		{"an engine subpackage", engineRoot + "/enginetest\tfalse", false},
		{"the engine under test", engineRoot + " [" + engineRoot + ".test]\tfalse", false},
		{"the test main", engineRoot + ".test\tfalse", false},
		{"the external tests", engineRoot + "_test [" + engineRoot + ".test]\tfalse", false},
		{"the herdr adapter", "github.com/swmcc/herdr-sightline/internal/herdr\tfalse", true},
		{"the config package", "github.com/swmcc/herdr-sightline/internal/config\tfalse", true},
		{"a third party module", "github.com/pelletier/go-toml/v2\tfalse", true},
		{"a package that only shares the prefix", engineRoot + "x\tfalse", true},
		{"golang.org/x is not the standard library", "golang.org/x/text\tfalse", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := outsideBoundary([]string{tt.line})
			if (len(got) > 0) != tt.bad {
				t.Fatalf("outsideBoundary(%q) = %q, want bad %v", tt.line, got, tt.bad)
			}
		})
	}
}

// outsideBoundary takes `go list` lines of "import path<tab>standard" and returns
// the import paths that are neither the standard library nor the engine.
func outsideBoundary(lines []string) []string {
	var bad []string
	for _, line := range lines {
		path, standard, _ := strings.Cut(line, "\t")
		if standard == "true" {
			continue
		}
		// Test variants look like "pkg [pkg.test]", the test main is "pkg.test" and
		// external tests are "pkg_test".
		path, _, _ = strings.Cut(path, " ")
		path = strings.TrimSuffix(strings.TrimSuffix(path, ".test"), "_test")
		if path == engineRoot || strings.HasPrefix(path, engineRoot+"/") {
			continue
		}
		if !slices.Contains(bad, path) {
			bad = append(bad, path)
		}
	}
	return bad
}
