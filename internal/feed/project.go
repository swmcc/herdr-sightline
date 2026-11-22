package feed

// Project files and trusting them (#22, #23). A repo can carry a
// .sightline.toml with presets, patterns and ignore rules for everyone who
// works on it. Whoever can push to the repo writes it, so it's untrusted input:
// it does nothing until the person has read it here and pressed y. Trust is for
// one version of the file, by its sha256, so any change means asking again.
//
// What's shown is the file as one read found it, and y trusts that read's sum.
// The rules that apply are always parsed from bytes with a trusted sum, so a
// file swapped after it was shown is just a new, untrusted version.

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/swmcc/herdr-sightline/internal/config"
)

// project is one repo's project file as the last read found it.
type project struct {
	// root is the repo's real path, with symlinks resolved. Trust is kept
	// against it.
	root string
	// sum is the sha256 of the bytes that were read and parsed.
	sum config.Sum
	// cfg is what the file adds. It's only set when err is nil.
	cfg config.Project
	// err is why the file can't be used at all. It's never trusted then.
	err error
	// trusted says this version of the file is trusted. trustedBefore says
	// another version was, so the feed can say it changed.
	trusted       bool
	trustedBefore bool
	// panes are the watched panes in the repo.
	panes []string
}

// path is the project file's full path.
func (p project) path() string { return filepath.Join(p.root, config.ProjectFileName) }

// projectsMsg is every project file the watched panes are in, sorted by root.
type projectsMsg struct {
	projects []project
	// trustErr is a problem with the trust file. Nothing is trusted then.
	trustErr error
}

// trustMsg is the result of trusting a project file, or forgetting it.
type trustMsg struct {
	root   string
	sum    config.Sum
	forget bool
	err    error
}

// trustState is the trust view: which project it shows, the sum it showed
// when it opened or the person last acknowledged a change, and how far down
// it's scrolled.
type trustState struct {
	root string
	sum  config.Sum
	top  int
	busy bool
}

// applyProjects takes a fresh look at the project files.
func (m *Model) applyProjects(msg projectsMsg) {
	m.projects, m.trustErr = msg.projects, msg.trustErr
	if m.mode == modeTrust {
		if _, ok := m.shownProject(); !ok {
			m.status = "That repo's project file has gone, or no watched pane is in the repo any more. Press esc to go back."
		}
	}
}

// shownProject is the project the trust view is showing.
func (m *Model) shownProject() (project, bool) {
	i := slices.IndexFunc(m.projects, func(p project) bool { return p.root == m.trust.root })
	if i < 0 {
		return project{}, false
	}
	return m.projects[i], true
}

// openTrust shows the first project file that needs a decision, or the first
// one there is.
func (m *Model) openTrust() {
	if len(m.projects) == 0 {
		m.status = "No watched pane is in a repo with a " + config.ProjectFileName + "."
		return
	}
	i := slices.IndexFunc(m.projects, func(p project) bool { return p.err == nil && !p.trusted })
	if i < 0 {
		i = 0
	}
	m.showProject(m.projects[i])
	m.mode = modeTrust
	m.status = ""
}

func (m *Model) showProject(p project) {
	m.trust.root, m.trust.sum, m.trust.top = p.root, p.sum, 0
}

// trustKey handles the trust view.
func (m *Model) trustKey(k tea.KeyMsg) tea.Cmd {
	p, ok := m.shownProject()
	page := max(m.height-4, 1)
	switch k.String() {
	case "esc", "q", "p":
		m.mode = modeList
		return nil
	case "j", "down":
		m.trust.top++
	case "k", "up":
		m.trust.top--
	case "pgdown", " ", "f":
		m.trust.top += page
	case "pgup", "b":
		m.trust.top -= page
	case "tab":
		if len(m.projects) > 0 {
			i := slices.IndexFunc(m.projects, func(q project) bool { return q.root == m.trust.root })
			m.showProject(m.projects[(i+1)%len(m.projects)])
			m.status = ""
		}
	case "y":
		if ok {
			return m.trustProject(p)
		}
	case "x":
		if ok {
			return m.forgetProject(p)
		}
	}
	m.trust.top = min(max(m.trust.top, 0), max(len(m.trustLines(p, max(m.width, 20)))-page, 0))
	return nil
}

// trustProject trusts the version of p on screen, once y is pressed.
func (m *Model) trustProject(p project) tea.Cmd {
	switch {
	case m.trust.busy:
		m.status = "Still recording the last decision."
		return nil
	case p.err != nil:
		m.status = "This file can't be used, so it can't be trusted. Fix it in the repo first."
		return nil
	case p.sum != m.trust.sum:
		// What's on screen now isn't what the person was reading, because the
		// file changed under them. Trusting it would trust something unread.
		m.trust.sum, m.trust.top = p.sum, 0
		m.status = "The file changed while you were reading it. Read it again, then press y."
		return nil
	case p.trusted:
		m.status = "This version is already trusted."
		return nil
	case m.stateDir == "":
		m.status = "There's no state directory to record trust in (HERDR_PLUGIN_STATE_DIR isn't set), so project files stay ignored."
		return nil
	}
	m.trust.busy = true
	m.status = "Trusting it..."
	dir, root, sum := m.stateDir, p.root, p.sum
	return func() tea.Msg {
		return trustMsg{root: root, sum: sum, err: config.TrustProject(dir, root, sum)}
	}
}

// forgetProject stops trusting p's repo, whatever version was trusted.
func (m *Model) forgetProject(p project) tea.Cmd {
	switch {
	case m.trust.busy:
		m.status = "Still recording the last decision."
		return nil
	case !p.trusted && !p.trustedBefore:
		m.status = "This repo's project file isn't trusted."
		return nil
	}
	m.trust.busy = true
	m.status = "Forgetting it..."
	dir, root := m.stateDir, p.root
	return func() tea.Msg {
		return trustMsg{root: root, forget: true, err: config.ForgetProject(dir, root)}
	}
}

// trustDone takes the result of trusting or forgetting. The projects loop
// reads the trust file again on its next look, which is what applies the
// change. Marking it here only keeps the screen in step until then.
func (m *Model) trustDone(msg trustMsg) {
	m.trust.busy = false
	if msg.err != nil {
		m.status = fmt.Sprintf("Not recorded: %v", msg.err)
		return
	}
	for i := range m.projects {
		p := &m.projects[i]
		if p.root != msg.root {
			continue
		}
		if msg.forget {
			p.trusted, p.trustedBefore = false, false
		} else if p.sum == msg.sum {
			p.trusted, p.trustedBefore = true, true
		}
	}
	if msg.forget {
		m.status = "Stopped trusting it. Its rules no longer apply."
		return
	}
	m.status = "Trusted this version. Its rules apply to panes in this repo from the next line."
}

// projectNotes are the header lines about project files that need the
// person: ones they haven't trusted and ones that can't be used.
func (m *Model) projectNotes() []string {
	var notes []string
	if m.trustErr != nil {
		notes = append(notes, fmt.Sprintf("No project file is trusted, because %v", m.trustErr))
	}
	for _, p := range m.projects {
		switch {
		case p.err != nil:
			notes = append(notes, fmt.Sprintf("%s can't be used, so it's ignored: %v", p.path(), firstLine(p.err.Error())))
		case p.trusted:
		case p.trustedBefore:
			notes = append(notes, fmt.Sprintf("%s has changed since you trusted it, so it's ignored. Press p to review it.", p.path()))
		default:
			notes = append(notes, fmt.Sprintf("%s isn't trusted, so it's ignored. Press p to review it.", p.path()))
		}
	}
	// The header cleans control characters. The rest is done here, since a
	// path or a rule quoted in an error comes from the repo.
	for i, n := range notes {
		notes[i] = visible(clean(n))
	}
	return notes
}

func firstLine(s string) string {
	first, rest, more := strings.Cut(s, "\n")
	if more && rest != "" {
		return first + " (and more, press p)"
	}
	return first
}

// trustView shows a project file's rules in full, so nothing is trusted
// unread, and says what trusting it risks.
func (m *Model) trustView() string {
	w := max(m.width, 20)
	h := max(m.height-4, 1)
	p, ok := m.shownProject()
	var lines []string
	if ok {
		lines = m.trustLines(p, w)
	}
	top := min(m.trust.top, max(len(lines)-h, 0))
	end := min(top+h, len(lines))

	head := "Project file"
	if len(m.projects) > 1 {
		i := slices.IndexFunc(m.projects, func(q project) bool { return q.root == m.trust.root })
		head = fmt.Sprintf("Project file %d of %d (tab for the next)", i+1, len(m.projects))
	}
	out := []string{faint.Render(fit(head, w)), ""}
	out = append(out, lines[top:end]...)
	for len(out) < h+2 {
		out = append(out, "")
	}
	keys := "y trust this version  x stop trusting  j/k scroll  esc back"
	if ok && p.err != nil {
		keys = "j/k scroll  esc back"
	}
	out = append(out, fit(clean(m.status), w), faint.Render(fit(keys, w)))
	return strings.Join(out, "\n")
}

// trustLines is everything the trust view says about p, wrapped to w so no
// part of a rule is cut off.
func (m *Model) trustLines(p project, w int) []string {
	var out []string
	add := func(s string) { out = append(out, wrap(visible(clean(s)), w)...) }
	// Styles are added after wrapping, so their escapes don't count as width.
	styled := func(style lipgloss.Style, s string) {
		for _, l := range wrap(visible(clean(s)), w) {
			out = append(out, style.Render(l))
		}
	}
	add(p.path())
	var names []string
	for _, pane := range p.panes {
		names = append(names, m.paneName(pane))
	}
	if len(names) > 0 {
		add("For the watched panes in this repo: " + strings.Join(names, ", "))
	}
	out = append(out, "")

	switch {
	case p.err != nil:
		styled(failStyle, "✗ This file can't be used, so nothing in it applies:")
		for _, l := range strings.Split(p.err.Error(), "\n") {
			add("  " + l)
		}
		return out
	case p.trusted:
		styled(okStyle, "✓ You trust this version. Its rules apply.")
	case p.trustedBefore:
		styled(newStyle, "● It has changed since you trusted it, so none of it applies until you trust this version.")
	default:
		styled(newStyle, "● You haven't trusted it, so none of it applies.")
	}
	out = append(out, "")

	c := p.cfg
	if len(c.Presets) == 0 && len(c.Patterns) == 0 && len(c.Ignores) == 0 {
		add("It adds nothing.")
	} else {
		add("It adds:")
	}
	if len(c.Presets) > 0 {
		note := ""
		if m.cfg.PresetsSet {
			note = " (left out: your sightline.toml sets presets, and yours win)"
		}
		add("  presets " + strings.Join(c.Presets, ", ") + note)
	}
	for _, pat := range c.Patterns {
		add(fmt.Sprintf("  error pattern %q: %s", pat.Name, pat.Regex.String()))
	}
	for _, ig := range c.Ignores {
		add("  ignore rule: " + ig.Regex.String())
	}
	out = append(out, "")
	add("Anyone who can push to this repo can change this file. Only trust it if you've read every rule above.")
	if len(c.Ignores) > 0 {
		out = append(out, "")
		styled(newStyle, "An ignore rule can hide a real error from you, and from the check on whether a fix worked.")
	}
	return out
}

// visible writes every character that doesn't show as itself as an escape,
// \x{202e} for a right to left override say. clean only deals with control
// characters, but a rule someone else wrote could use a format character or an
// invisible one to read as something it isn't, and this is where it's read
// before it's trusted.
func visible(s string) string {
	if !strings.ContainsFunc(s, unseen) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if unseen(r) {
			fmt.Fprintf(&b, `\x{%x}`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// unseen reports whether r wouldn't show as itself: anything but a space that
// isn't printable, and format characters such as bidi overrides and zero
// width joiners.
func unseen(r rune) bool {
	return r == utf8.RuneError || (r != ' ' && !unicode.IsPrint(r)) || unicode.Is(unicode.Cf, r)
}

// wrap breaks s into lines of at most w cells, so long text is shown whole.
func wrap(s string, w int) []string {
	var out []string
	for runewidth.StringWidth(s) > w {
		part := runewidth.Truncate(s, w, "")
		if part == "" {
			break
		}
		out = append(out, part)
		s = s[len(part):]
	}
	return append(out, s)
}
