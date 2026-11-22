package feed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// recompileLine is proof that something ran after a fix.
const recompileLine = "compiled successfully in 80 ms"

const repoFile = `[[pattern]]
name = "our-framework"
regex = '^FATAL \[app\] .+'

[[ignore]]
regex = '^Error: flaky upstream'
`

// repoHarness is a feed watching the web pane, which works in a real repo
// with a project file in it.
func repoHarness(t *testing.T, file string) (h *harness, repo, stateDir string) {
	t.Helper()
	h, stateDir = trustHarness(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo = filepath.Join(base, "web")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProject(t, repo, file)
	h.herdr.mu.Lock()
	h.herdr.cwd = filepath.Join(repo, "app")
	h.herdr.mu.Unlock()
	h.sync()
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Initial: true, Lines: []string{"ready"}}})
	return h, repo, stateDir
}

func writeProject(t *testing.T, repo, data string) {
	t.Helper()
	if err := os.WriteFile(config.ProjectPath(repo), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// look runs the projects loop's read once and hands the result to the model,
// as the loop would.
func (h *harness) look(stateDir string) {
	h.t.Helper()
	h.update(readProjects(h.loops.currentCwds(), stateDir, map[parseKey]parsedProject{}))
}

// print feeds lines to the web pane and lets their captures finish.
func (h *harness) print(lines ...string) {
	h.t.Helper()
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: lines}})
	h.clock.Advance(2 * time.Second)
	h.update(tickMsg{})
}

func (h *harness) firstLines() []string {
	var out []string
	for _, en := range h.model.entries {
		out = append(out, en.Lines[0])
	}
	return out
}

func TestProjectFileOnlyAppliesOnceTrusted(t *testing.T) {
	h, repo, stateDir := repoHarness(t, repoFile)
	if got := h.loops.currentCwds()[webPane]; got != filepath.Join(repo, "app") {
		t.Fatalf("the projects loop was handed %q for the web pane", got)
	}
	h.look(stateDir)
	if len(h.model.projects) != 1 || h.model.projects[0].root != repo {
		t.Fatalf("projects = %+v, want the one in %s", h.model.projects, repo)
	}

	// Untrusted, it does nothing at all.
	h.print("FATAL [app] database gone", "Error: flaky upstream timed out")
	if got := h.firstLines(); len(got) != 1 || got[0] != "Error: flaky upstream timed out" {
		t.Fatalf("untrusted, entries = %q, want only the generic error", got)
	}

	h.press("p")
	h.run(h.press("y"))
	h.press("esc")
	h.look(stateDir)
	if !h.model.projects[0].trusted {
		t.Fatalf("not trusted after y (status %q)", h.model.status)
	}

	h.print("FATAL [app] database gone again", "Error: flaky upstream timed out again")
	got := h.firstLines()
	if len(got) != 2 || got[0] != "FATAL [app] database gone again" {
		t.Fatalf("trusted, entries = %q, want the repo's pattern to catch FATAL and its rule to hide the flaky error", got)
	}
	view := h.model.View()
	if !strings.Contains(view, "Project ignore rules have hidden 1 error. Press h to see them.") || !strings.Contains(view, "h hidden") {
		t.Errorf("the list doesn't say an error was hidden:\n%s", view)
	}
	h.press("h")
	if hv := h.model.View(); !strings.Contains(hv, "Error: flaky upstream timed out again") || !strings.Contains(hv, "web · ") {
		t.Errorf("h doesn't list the hidden error and its pane:\n%s", hv)
	}
	h.press("esc")

	// Swapping the file after trusting it is a new version, so nothing in it
	// applies, the new rule least of all.
	writeProject(t, repo, repoFile+"\n[[ignore]]\nregex = '^FATAL \\[app\\] database'\n")
	h.look(stateDir)
	if p := h.model.projects[0]; p.trusted || !p.trustedBefore {
		t.Fatalf("a changed file is %+v, want untrusted and known to have changed", p)
	}
	h.print("FATAL [app] database gone a third time", "Error: flaky upstream once more")
	if got := h.firstLines(); got[0] != "Error: flaky upstream once more" || len(got) != 3 {
		t.Errorf("after a swap, entries = %q, want the old rules gone and the new one ignored", got)
	}
}

// A project file only reaches panes in its own repo.
// trustedRepo is repoHarness with the project file already trusted and
// looked at, so its rules are on the web pane.
func trustedRepo(t *testing.T) (h *harness, repo, stateDir string) {
	t.Helper()
	h, repo, stateDir = repoHarness(t, repoFile)
	if err := config.TrustProject(stateDir, repo, config.SumOf([]byte(repoFile))); err != nil {
		t.Fatal(err)
	}
	h.look(stateDir)
	if !h.model.projects[0].trusted {
		t.Fatal("not trusted")
	}
	return h, repo, stateDir
}

// sentAndSettling catches a repo pattern's error on the web pane, sends it and
// lets the agent finish, so its fix is being checked.
func (h *harness) sentAndSettling() {
	h.t.Helper()
	h.print("FATAL [app] database gone")
	h.run(h.press("enter"))
	if h.model.entries[0].State != engine.StateSent {
		h.t.Fatalf("not sent (status %q)", h.model.status)
	}
	h.herdr.setStatus(agentPane, herdr.AgentWorking)
	h.sync()
	h.herdr.setStatus(agentPane, herdr.AgentIdle)
	h.sync()
	h.clock.Advance(2 * time.Second)
}

// Looking again and finding the same trusted file changes nothing, so it
// mustn't cost a fix check its verdict.
func TestTheSameProjectFileTwiceKeepsAFix(t *testing.T) {
	h, _, stateDir := trustedRepo(t)
	h.sentAndSettling()
	h.look(stateDir)
	h.look(stateDir)
	h.print(recompileLine)
	h.clock.Advance(31 * time.Second)
	h.update(tickMsg{})
	if got := h.model.entries[0].State; got != engine.StateFixed {
		t.Errorf("state = %v, want fixed", got)
	}
}

// The file changing under a fix check takes its pattern away, so the error
// coming back couldn't be seen. The check can't say fixed.
func TestAProjectFileChangingStopsAFix(t *testing.T) {
	h, repo, stateDir := trustedRepo(t)
	h.sentAndSettling()
	writeProject(t, repo, repoFile+"# changed\n")
	h.look(stateDir)
	h.print(recompileLine, "FATAL [app] database gone")
	h.clock.Advance(31 * time.Second)
	h.update(tickMsg{})
	if got := h.model.entries[0].State; got != engine.StateUnverified {
		t.Errorf("state = %v, want unverified", got)
	}
}

// A pane that moves keeps its repo's rules, and one that leaves the repo
// loses them at once.
func TestProjectRulesFollowThePane(t *testing.T) {
	h, _, _ := trustedRepo(t)
	moved := &herdr.Pane{ID: "w2:p1", WorkspaceID: "w2", TabID: "w2:t1"}
	h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneMoved, PaneID: "w2:p1", PreviousPaneID: webPane, Pane: moved}})
	if got := h.loops.currentCwds()["w2:p1"]; got == "" {
		t.Error("the moved pane's working directory wasn't kept")
	}
	h.update(deltaMsg{pane: "w2:p1", gen: h.gen("w2:p1"), delta: herdr.Delta{Initial: true}})
	h.update(deltaMsg{pane: "w2:p1", gen: h.gen("w2:p1"), delta: herdr.Delta{Lines: []string{"Error: flaky upstream after the move"}}})
	h.clock.Advance(2 * time.Second)
	h.update(tickMsg{})
	if total, _ := h.model.engine.Hidden(); total != 1 {
		t.Fatalf("after a move hidden = %d, want the rule still on the pane", total)
	}

	// Back to the web pane in a fresh harness, leaving the repo.
	h, _, _ = trustedRepo(t)
	h.herdr.mu.Lock()
	h.herdr.cwd = t.TempDir()
	h.herdr.mu.Unlock()
	h.sync()
	h.print("Error: flaky upstream elsewhere")
	if total, _ := h.model.engine.Hidden(); total != 0 {
		t.Errorf("a pane that left the repo still had its rules: hidden %d", total)
	}
}

// Trusting from the feed applies straight away, not at the next look.
func TestTrustAppliesAtOnce(t *testing.T) {
	h, _, stateDir := repoHarness(t, repoFile)
	h.look(stateDir)
	h.press("p")
	h.run(h.press("y"))
	h.press("esc")
	h.print("Error: flaky upstream right after")
	if total, _ := h.model.engine.Hidden(); total != 1 {
		t.Errorf("hidden = %d straight after y, want 1", total)
	}
	h.press("p")
	h.run(h.press("x"))
	h.press("esc")
	h.print("Error: flaky upstream after forgetting")
	if total, _ := h.model.engine.Hidden(); total != 1 {
		t.Errorf("hidden = %d after x, want still 1", total)
	}
}

func TestProjectRulesStayInTheirRepo(t *testing.T) {
	h, repo, stateDir := repoHarness(t, repoFile)
	sum := config.SumOf([]byte(repoFile))
	if err := config.TrustProject(stateDir, repo, sum); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	h.herdr.mu.Lock()
	h.herdr.cwd = other
	h.herdr.mu.Unlock()
	h.sync()
	h.look(stateDir)
	if len(h.model.projects) != 0 {
		t.Fatalf("a pane outside the repo found %+v", h.model.projects)
	}
	h.print("FATAL [app] database gone")
	if len(h.model.entries) != 0 {
		t.Errorf("the repo's pattern reached a pane outside it: %q", h.firstLines())
	}
}

func TestProjectPresetsOnlyAdd(t *testing.T) {
	all := config.Default()
	chosen := config.Default()
	chosen.Presets, chosen.PresetsSet = []string{"node"}, true
	project := config.Project{Presets: []string{"go"}, PresetsSet: true}
	none := config.Project{Presets: []string{}, PresetsSet: true}

	tests := []struct {
		name string
		user config.Config
		p    config.Project
		want int // matchers
	}{
		{"presets the person already has add nothing", all, project, 0},
		{"the person's own list wins", chosen, project, 0},
		{"an empty list can't switch the person's presets off", all, none, 0},
	}
	for _, tt := range tests {
		if got := len(projectRules(tt.user, tt.p).Matchers); got != tt.want {
			t.Errorf("%s: %d matchers, want %d", tt.name, got, tt.want)
		}
	}
}
