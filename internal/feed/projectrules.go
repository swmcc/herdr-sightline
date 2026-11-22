package feed

// Applying trusted project files (#22) and showing what their ignore rules
// hid. See project.go for finding them and trusting them.

import (
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/engine"
)

// handCwds tells the projects loop where the watched panes are working, from
// the last snapshot.
func (m *Model) handCwds() {
	if m.loops == nil {
		return
	}
	cwds := map[string]string{}
	for _, pane := range m.watching {
		if p, ok := m.panes[pane]; ok && p.Cwd != "" {
			cwds[pane] = p.Cwd
		}
	}
	m.loops.noteCwds(cwds)
}

// applyProjectRules gives each watched pane the rules of its repo's project
// file when that version of it is trusted, and none otherwise.
func (m *Model) applyProjectRules() {
	for _, pane := range m.watching {
		var rules engine.Rules
		for _, p := range m.projects {
			if p.trusted && p.err == nil && slices.Contains(p.panes, pane) {
				rules = projectRules(m.cfg, p.cfg)
				break
			}
		}
		m.engine.SetRules(engine.SourceID(pane), rules)
	}
}

// projectRules turns a project file into rules for its panes. The person's own
// sightline.toml wins: the project's patterns and ignore rules only add to
// theirs, and its presets only switch on ones theirs leaves out, and only when
// theirs doesn't list presets at all.
func projectRules(user config.Config, p config.Project) engine.Rules {
	var rules engine.Rules
	if p.PresetsSet && !user.PresetsSet {
		var extra []string
		for _, name := range p.Presets {
			if !slices.Contains(user.Presets, name) {
				extra = append(extra, name)
			}
		}
		// The names were checked when the file was parsed, so this can't fail.
		if ms, err := engine.Presets(extra...); err == nil && len(extra) > 0 {
			rules.Matchers = ms
		}
	}
	for _, pat := range p.Patterns {
		rules.Matchers = append(rules.Matchers, engine.Regexp("project/"+pat.Name, pat.Regex))
	}
	for _, ig := range p.Ignores {
		rules.Ignores = append(rules.Ignores, engine.Regexp("project ignore", ig.Regex))
	}
	return rules
}

// hiddenNote is the header line saying project rules have hidden errors, or ""
// when they haven't.
func (m *Model) hiddenNote() string {
	total, _ := m.engine.Hidden()
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("Project ignore rules have hidden %s. Press h to see them.", plural(total, "error", "errors"))
}

// openHidden lists what project ignore rules hid.
func (m *Model) openHidden() {
	if total, _ := m.engine.Hidden(); total == 0 {
		m.status = "No project ignore rule has hidden anything."
		return
	}
	m.mode, m.hiddenTop, m.status = modeHidden, 0, ""
}

// hiddenKey handles the list of hidden errors.
func (m *Model) hiddenKey(k tea.KeyMsg) {
	switch k.String() {
	case "esc", "q", "h":
		m.mode = modeList
	case "j", "down":
		m.hiddenTop++
	case "k", "up":
		m.hiddenTop--
	}
	_, recent := m.engine.Hidden()
	m.hiddenTop = min(max(m.hiddenTop, 0), max(len(recent)-1, 0))
}

// hiddenView lists the errors project ignore rules kept out of the feed,
// newest first, so a rule hiding more than it should is seen.
func (m *Model) hiddenView() string {
	w := max(m.width, 20)
	h := max(m.height-4, 1)
	total, recent := m.engine.Hidden()
	head := fmt.Sprintf("Errors hidden by project ignore rules · %d", total)
	if total > len(recent) {
		head += fmt.Sprintf(", the latest %d shown", len(recent))
	}
	out := []string{faint.Render(fit(head, w)), ""}
	var lines []string
	for i := len(recent) - 1; i >= 0; i-- {
		hd := recent[i]
		lines = append(lines, faint.Render(fit(m.paneName(string(hd.Source))+" · "+ago(hd.At, m.now()), w)))
		lines = append(lines, fit("  "+clean(hd.Line), w))
	}
	start := min(m.hiddenTop*2, max(len(lines)-1, 0))
	end := min(start+h, len(lines))
	out = append(out, lines[start:end]...)
	for len(out) < h+2 {
		out = append(out, "")
	}
	out = append(out, fit(clean(m.status), w), faint.Render(fit("j/k scroll  esc back", w)))
	return strings.Join(out, "\n")
}
