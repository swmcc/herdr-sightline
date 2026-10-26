package feed

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/engine"
)

// neverHarness is a feed with a config directory and one entry caught.
func neverHarness(t *testing.T) (*harness, string, engine.Entry) {
	t.Helper()
	h := newHarness(t)
	dir := t.TempDir()
	h.model.configDir = dir
	return h, dir, h.catchError()
}

func (h *harness) typeText(s string) {
	h.t.Helper()
	for _, r := range s {
		if cmd := h.press(string(r)); cmd != nil {
			h.t.Fatalf("typing %q gave a command", r)
		}
	}
}

func fileText(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(config.Path(dir))
	if err != nil {
		t.Fatalf("reading sightline.toml: %v", err)
	}
	return string(data)
}

func TestNeverAgainAddsARuleAndUndoesIt(t *testing.T) {
	h, dir, en := neverHarness(t)

	h.press("n")
	if h.model.mode != modeNever {
		t.Fatalf("mode = %v, want the rule editor", h.model.mode)
	}
	if got, want := string(h.model.never.text), suggestRule(en.Lines[0]); got != want {
		t.Errorf("editor starts with %q, want the suggestion %q", got, want)
	}
	// The suggestion came from the pane. Nothing is written until enter.
	if _, err := os.Stat(config.Path(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("n wrote the file before enter (stat: %v)", err)
	}
	view := h.model.View()
	if !strings.Contains(view, "Compiles and matches") || !strings.Contains(view, "Enter adds it to the end of "+dir[:20]) {
		t.Errorf("the editor doesn't say the rule is fine and where it goes:\n%s", view)
	}

	h.update(tea.KeyMsg{Type: tea.KeyCtrlU})
	h.typeText("^TypeError: Cannot read")
	h.run(h.press("enter"))
	if h.model.mode != modeList {
		t.Errorf("mode = %v after saving, want the list", h.model.mode)
	}
	if !strings.Contains(h.model.status, "Added an ignore rule") {
		t.Errorf("status = %q", h.model.status)
	}
	want := "# added from the feed on 2026-01-01\n[[ignore]]\nregex = '^TypeError: Cannot read'\n"
	if got := fileText(t, dir); got != want {
		t.Errorf("sightline.toml is %q, want %q", got, want)
	}
	// The feed doesn't apply the rule itself. The reload from the config watch does.
	if len(h.model.entries) != 1 {
		t.Fatalf("the entry went before the reload")
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	h.update(configMsg{config.Update{Config: cfg}})
	if len(h.model.entries) != 0 {
		t.Errorf("the reload left %d entries, want the rule to dismiss the one it was for", len(h.model.entries))
	}
	for _, s := range []string{"Ignore rule added.", "dismissed 1 entry", "Press u within 30s"} {
		if !strings.Contains(h.model.status, s) {
			t.Errorf("status = %q, want it to contain %q", h.model.status, s)
		}
	}
	// A later reload is just a reload.
	h.update(configMsg{config.Update{Config: cfg}})
	if h.model.status != "Reloaded sightline.toml." {
		t.Errorf("second reload status = %q", h.model.status)
	}

	h.clock.Advance(10 * time.Second)
	h.run(h.press("u"))
	if _, err := os.Stat(config.Path(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after undo sightline.toml is still there (stat: %v), want it gone as before", err)
	}
	if !strings.Contains(h.model.status, "Removed the rule") {
		t.Errorf("undo status = %q", h.model.status)
	}
	// The reload after the undo still says so.
	h.update(configMsg{config.Update{Config: config.Default()}})
	if h.model.status != "Rule removed. Reloaded sightline.toml." {
		t.Errorf("reload after undo status = %q", h.model.status)
	}
	if cmd := h.press("u"); cmd != nil {
		t.Error("a second u tried to undo again")
	}
}

func TestNeverAgainEditor(t *testing.T) {
	h, dir, _ := neverHarness(t)
	h.press("n")

	// Keys that do something in the list just type here.
	h.update(tea.KeyMsg{Type: tea.KeyCtrlU})
	h.typeText("qdx")
	h.update(tea.KeyMsg{Type: tea.KeyLeft})
	h.update(tea.KeyMsg{Type: tea.KeyBackspace})
	h.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a\nb\x1b"), Paste: true})
	if got := string(h.model.never.text); got != "qabx" {
		t.Errorf("text = %q, want %q (line breaks and escapes from a paste dropped)", got, "qabx")
	}

	// A rule that would hide everything isn't saved, and the editor stays.
	h.update(tea.KeyMsg{Type: tea.KeyCtrlU})
	h.update(tea.KeyMsg{Type: tea.KeyCtrlK})
	h.typeText(".")
	if cmd := h.press("enter"); cmd != nil {
		t.Error("enter on a bad rule gave a command")
	}
	if h.model.mode != modeNever || !strings.HasPrefix(h.model.status, "Not saved.") {
		t.Errorf("mode %v, status %q, want to stay in the editor and say why", h.model.mode, h.model.status)
	}

	h.press("esc")
	if h.model.mode != modeList {
		t.Errorf("esc left mode %v", h.model.mode)
	}
	if _, err := os.Stat(config.Path(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("nothing should have been written (stat: %v)", err)
	}
}

// While the save runs the editor stays put, so its result can't land on a
// different rule or contradict a "no rule added".
func TestNeverAgainWaitsForTheSave(t *testing.T) {
	h, dir, _ := neverHarness(t)
	h.press("n")
	save := h.press("enter")
	if save == nil {
		t.Fatal("enter didn't save")
	}
	h.press("esc")
	h.press("x")
	if h.model.mode != modeNever || h.model.status != "Still saving the rule." {
		t.Errorf("mode %v, status %q, want the editor to wait for the save", h.model.mode, h.model.status)
	}
	h.run(save)
	if h.model.mode != modeList || !strings.Contains(h.model.status, "Added an ignore rule") {
		t.Errorf("mode %v, status %q after the save", h.model.mode, h.model.status)
	}
	if !strings.Contains(fileText(t, dir), "[[ignore]]") {
		t.Error("the rule wasn't written")
	}
}

func TestNeverAgainLeavesABrokenFileAlone(t *testing.T) {
	h, dir, _ := neverHarness(t)
	const broken = "settle_seconds = \n"
	if err := os.WriteFile(config.Path(dir), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	h.press("n")
	h.run(h.press("enter"))
	if h.model.mode != modeNever || !strings.Contains(h.model.status, "already has a problem") {
		t.Errorf("mode %v, status %q, want to stay in the editor and say the file is broken", h.model.mode, h.model.status)
	}
	if got := fileText(t, dir); got != broken {
		t.Errorf("the broken file was changed to %q", got)
	}
}

func TestNeverAgainUndoIsCareful(t *testing.T) {
	tests := []struct {
		name   string
		before func(t *testing.T, h *harness, dir string)
		want   string
	}{
		{"too late", func(t *testing.T, h *harness, dir string) {
			h.clock.Advance(undoFor + time.Second)
		}, "Too late to undo"},
		{"file changed", func(t *testing.T, h *harness, dir string) {
			f, err := os.OpenFile(config.Path(dir), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString("# mine\n"); err != nil {
				t.Fatal(err)
			}
		}, "Can't undo safely"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, dir, _ := neverHarness(t)
			h.press("n")
			h.run(h.press("enter"))
			tt.before(t, h, dir)
			before := fileText(t, dir)
			h.run(h.press("u"))
			if !strings.Contains(h.model.status, tt.want) {
				t.Errorf("status = %q, want it to contain %q", h.model.status, tt.want)
			}
			if got := fileText(t, dir); got != before {
				t.Errorf("a refused undo changed the file from %q to %q", before, got)
			}
			if cmd := h.press("u"); cmd != nil {
				t.Error("u still offers to undo after refusing")
			}
		})
	}
}
