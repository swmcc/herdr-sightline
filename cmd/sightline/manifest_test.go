package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// manifest is the part of herdr-plugin.toml this test checks.
type manifest struct {
	ID              string   `toml:"id"`
	Name            string   `toml:"name"`
	Version         string   `toml:"version"`
	MinHerdrVersion string   `toml:"min_herdr_version"`
	Platforms       []string `toml:"platforms"`
	Actions         []struct {
		ID      string   `toml:"id"`
		Command []string `toml:"command"`
	} `toml:"actions"`
	Panes []struct {
		ID      string   `toml:"id"`
		Command []string `toml:"command"`
	} `toml:"panes"`
}

// herdr runs whatever the manifest says, so a typo in a command only shows up when
// someone presses the key. This catches it here instead.
func TestManifestPointsAtCommandsThatExist(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "herdr-plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := toml.Unmarshal(b, &m); err != nil {
		t.Fatalf("herdr-plugin.toml doesn't parse: %v", err)
	}

	if m.ID != "swmcc.sightline" {
		t.Errorf("id = %q, want %q", m.ID, "swmcc.sightline")
	}
	for field, value := range map[string]string{"name": m.Name, "version": m.Version, "min_herdr_version": m.MinHerdrVersion} {
		if value == "" {
			t.Errorf("%s is missing", field)
		}
	}
	if len(m.Platforms) == 0 {
		t.Error("platforms is missing")
	}
	if len(m.Actions) == 0 || len(m.Panes) == 0 {
		t.Fatalf("got %d actions and %d panes, want at least one of each", len(m.Actions), len(m.Panes))
	}

	known := subcommands(t)
	check := func(kind, id string, command []string) {
		t.Run(kind+" "+id, func(t *testing.T) {
			if len(command) != 2 {
				t.Fatalf("command = %q, want bin/sightline and one subcommand", command)
			}
			if command[0] != "bin/sightline" {
				t.Errorf("command runs %q, want bin/sightline", command[0])
			}
			if !known[command[1]] {
				t.Errorf("subcommand %q isn't one the binary handles (it handles %v)", command[1], keys(known))
			}
		})
	}
	for _, a := range m.Actions {
		check("action", a.ID, a.Command)
	}
	for _, p := range m.Panes {
		check("pane", p.ID, p.Command)
	}
}

// subcommands reads the commands from the binary's own help rather than keeping a
// second list here that could fall out of step with it.
func subcommands(t *testing.T) map[string]bool {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, func(string) string { return "" }, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("sightline help exited %d: %s", code, stderr.String())
	}
	_, list, ok := strings.Cut(stdout.String(), "\nCommands:\n")
	if !ok {
		t.Fatalf("help has no Commands section:\n%s", stdout.String())
	}
	known := map[string]bool{}
	for _, line := range strings.Split(list, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			known[fields[0]] = true
		}
	}
	if len(known) == 0 {
		t.Fatalf("help lists no commands:\n%s", stdout.String())
	}
	return known
}

func keys(m map[string]bool) []string {
	return slices.Sorted(maps.Keys(m))
}
