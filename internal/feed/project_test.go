package feed

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swmcc/herdr-sightline/internal/config"
)

const repoRoot = "/home/me/code/web"

// projectFrom parses a project file the way the projects loop does.
func projectFrom(t *testing.T, data string) project {
	t.Helper()
	p := project{root: repoRoot, sum: config.SumOf([]byte(data)), panes: []string{webPane}}
	p.cfg, p.err = config.ParseProject(".sightline.toml", []byte(data))
	return p
}

const projectFile = `presets = ["go"]

[[pattern]]
name = "our-framework"
regex = '^FATAL \[app\] .+'

[[ignore]]
regex = 'DeprecationWarning: Buffer\(\) is deprecated'
`

func trustHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	dir := t.TempDir()
	h.model.stateDir = dir
	h.sync()
	h.update(watchListMsg{panes: []string{webPane}})
	return h, dir
}

func trusted(t *testing.T, dir string, p project) bool {
	t.Helper()
	trust, err := config.LoadTrust(dir)
	if err != nil {
		t.Fatal(err)
	}
	return trust.Trusted(p.root, p.sum)
}

func TestTrustingAProjectFile(t *testing.T) {
	h, dir := trustHarness(t)
	p := projectFrom(t, projectFile)
	h.update(projectsMsg{projects: []project{p}})

	list := h.model.View()
	if !strings.Contains(list, repoRoot+"/.sightline.toml isn't trusted, so it's ignored. Press p to review it.") {
		t.Errorf("the list doesn't say there's a file to review:\n%s", list)
	}
	if !strings.Contains(list, "p project") {
		t.Errorf("the keys don't mention p:\n%s", list)
	}

	h.press("p")
	if h.model.mode != modeTrust {
		t.Fatalf("mode = %v, want the trust view", h.model.mode)
	}
	view := h.model.View()
	for _, want := range []string{
		"You haven't trusted it, so none of it applies.",
		"presets go",
		`error pattern "our-framework": ^FATAL \[app\] .+`,
		`ignore rule: DeprecationWarning: Buffer\(\) is deprecated`,
		"Anyone who can push to this repo can change this file.",
		"An ignore rule can hide a real error",
		"y trust this version",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the trust view doesn't say %q:\n%s", want, view)
		}
	}
	if trusted(t, dir, p) {
		t.Fatal("trusted before y")
	}

	h.run(h.press("y"))
	if !trusted(t, dir, p) {
		t.Fatalf("y didn't record trust (status %q)", h.model.status)
	}
	if !h.model.projects[0].trusted {
		t.Error("the feed doesn't show it trusted")
	}
	if !strings.Contains(h.model.status, "Trusted this version") {
		t.Errorf("status = %q", h.model.status)
	}

	// x takes it back.
	h.run(h.press("x"))
	if trusted(t, dir, p) || h.model.projects[0].trusted {
		t.Errorf("x didn't stop trusting it (status %q)", h.model.status)
	}
	h.press("esc")
	if h.model.mode != modeList {
		t.Errorf("esc left mode %v", h.model.mode)
	}
}

// The view shows one read of the file. If a later read changes it while the
// person is reading, y mustn't trust a version they haven't seen.
func TestTrustRefusesAFileThatChangedWhileShown(t *testing.T) {
	h, dir := trustHarness(t)
	before := projectFrom(t, projectFile)
	h.update(projectsMsg{projects: []project{before}})
	h.press("p")

	after := projectFrom(t, projectFile+"\n[[ignore]]\nregex = '^WARN flaky test skipped$'\n")
	h.update(projectsMsg{projects: []project{after}})
	if cmd := h.press("y"); cmd != nil {
		t.Fatal("y trusted a file that changed after it was shown")
	}
	if !strings.Contains(h.model.status, "changed while you were reading it") {
		t.Errorf("status = %q", h.model.status)
	}
	if !strings.Contains(h.model.View(), "^WARN flaky test skipped") {
		t.Errorf("the view doesn't show the new rule")
	}
	h.run(h.press("y"))
	if trusted(t, dir, before) || !trusted(t, dir, after) {
		t.Errorf("the second y should trust the version now on screen, and only that")
	}

	// A further change after trusting is a new version, so it's untrusted again.
	again := projectFrom(t, projectFile+"# a comment\n")
	again.trustedBefore = true
	h.press("esc")
	h.update(projectsMsg{projects: []project{again}})
	if !strings.Contains(h.model.View(), "has changed since you trusted it, so it's ignored") {
		t.Errorf("the list doesn't say the file changed:\n%s", h.model.View())
	}
}

func TestTrustRefuses(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		stateDir bool
		want     string
	}{
		{"a file that can't be used", "settle_seconds = 1\n", true, "can't be used, so it can't be trusted"},
		{"nowhere to record it", projectFile, false, "HERDR_PLUGIN_STATE_DIR isn't set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, dir := trustHarness(t)
			if !tt.stateDir {
				h.model.stateDir = ""
			}
			p := projectFrom(t, tt.data)
			h.update(projectsMsg{projects: []project{p}})
			h.press("p")
			if cmd := h.press("y"); cmd != nil {
				t.Fatal("y gave a command")
			}
			if !strings.Contains(h.model.status, tt.want) {
				t.Errorf("status = %q, want it to say %q", h.model.status, tt.want)
			}
			if trusted(t, dir, p) {
				t.Error("trusted anyway")
			}
		})
	}
}

// A rule someone else wrote is shown as it really is. Escapes can't act on the
// terminal and a bidi override or a zero width character can't make it read as
// something else.
func TestTrustViewShowsHiddenCharacters(t *testing.T) {
	h, _ := trustHarness(t)
	data := "[[ignore]]\nregex = \"^WARN \\u202Eeman\\u200B x\\u001b\\\\[2J$\"\n"
	p := projectFrom(t, data)
	if p.err != nil {
		t.Fatal(p.err)
	}
	h.update(projectsMsg{projects: []project{p}})
	h.press("p")
	view := h.model.View()
	for _, bad := range []string{"\u202e", "\u200b", "\x1b"} {
		if strings.Contains(view, bad) {
			t.Errorf("the view has %q in it raw", bad)
		}
	}
	for _, want := range []string{`\x{202e}`, `\x{200b}`, `\x1b\[2J`} {
		if !strings.Contains(view, want) {
			t.Errorf("the view doesn't show %s:\n%s", want, view)
		}
	}
}

func TestProjectNotes(t *testing.T) {
	h, _ := trustHarness(t)
	broken := projectFrom(t, "[[watch]]\npane_title = \"web\"\nagent = \"codex\"\n")
	h.update(projectsMsg{projects: []project{broken}, trustErr: errTest("trusted-projects.json is broken")})
	view := h.model.View()
	for _, want := range []string{
		"No project file is trusted, because trusted-projects.json is broken",
		repoRoot + "/.sightline.toml can't be used, so it's ignored: .sightline.toml:1:3: watch can't be set",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the list doesn't say %q:\n%s", want, view)
		}
	}
	h.press("p")
	if !strings.Contains(h.model.View(), "This file can't be used, so nothing in it applies") {
		t.Errorf("the trust view doesn't say the file can't be used:\n%s", h.model.View())
	}
	if strings.Contains(h.model.View(), "y trust") {
		t.Error("the trust view offers y on a file that can't be used")
	}
}

func TestTrustViewScrollsAndSwitches(t *testing.T) {
	h, _ := trustHarness(t)
	var b strings.Builder
	for i := range 40 {
		b.WriteString("[[ignore]]\nregex = '^WARN rule " + strings.Repeat("x", i) + "$'\n")
	}
	one := projectFrom(t, b.String())
	two := projectFrom(t, projectFile)
	two.root = "/home/me/code/api"
	h.update(projectsMsg{projects: []project{two, one}})
	h.press("p")
	if h.model.trust.root != two.root {
		t.Fatalf("opened on %q, want the first untrusted file", h.model.trust.root)
	}
	h.update(tea.KeyMsg{Type: tea.KeyTab})
	if h.model.trust.root != one.root {
		t.Fatalf("tab went to %q", h.model.trust.root)
	}
	if strings.Contains(h.model.View(), "x$") && strings.Contains(h.model.View(), strings.Repeat("x", 39)+"$") {
		t.Error("everything fits, so the test proves nothing about scrolling")
	}
	for range 100 {
		h.press("j")
	}
	if !strings.Contains(h.model.View(), strings.Repeat("x", 39)+"$") {
		t.Errorf("scrolling to the end doesn't show the last rule:\n%s", h.model.View())
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
