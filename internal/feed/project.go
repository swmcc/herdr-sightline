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
	"strconv"
	"strings"
	"time"
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

// trustState is the trust view: which project it shows, the sum of the
// version it's showing, how far down it's scrolled and whether the end of that
// version has been on screen. y only trusts a version read to the end.
type trustState struct {
	root    string
	sum     config.Sum
	top     int
	seenEnd bool
	busy    bool
	// changedAt is when a new version replaced the one being read. y waits a
	// moment after that, so a press meant for the old one can't trust the new.
	changedAt time.Time
}

// changeSettle is how long y is refused after the file changes on screen.
const changeSettle = 3 * time.Second

// applyProjects takes a fresh look at the project files.
func (m *Model) applyProjects(msg projectsMsg) {
	m.projects, m.trustErr = msg.projects, msg.trustErr
	if m.mode != modeTrust {
		return
	}
	p, ok := m.shownProject()
	switch {
	case !ok:
		m.status = "That repo's project file has gone, or no watched pane is in the repo any more. Press esc to go back."
	case p.sum != m.trust.sum:
		// A new version replaces what was being read. It's read from the top,
		// all of it, before y trusts it, and the person is told straight away
		// rather than at the next y.
		m.showProject(p)
		m.trust.changedAt = m.now()
		m.status = "The file just changed. This is the new version, so read it from the top before pressing y."
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
	m.trust.root, m.trust.sum, m.trust.top, m.trust.seenEnd = p.root, p.sum, 0, false
	m.noteTrustSeen()
}

// The smallest pane the trust view draws properly. Below this the terminal
// cuts lines or Bubble Tea drops the body to keep the footer, so the end
// can't count as seen and nothing can be trusted.
const (
	trustMinWidth  = 20
	trustMinHeight = 8
)

func (m *Model) tooSmallToTrust() bool {
	return m.width < trustMinWidth || m.height < trustMinHeight
}

// trustHeight is how many lines of the trust view fit on screen.
func (m *Model) trustHeight() int { return max(m.height-4, 1) }

// noteTrustSeen notes when the last line of the version shown is on screen.
// The model calls it after anything that could scroll or resize the view.
func (m *Model) noteTrustSeen() {
	p, ok := m.shownProject()
	if !ok || p.sum != m.trust.sum || m.tooSmallToTrust() {
		return
	}
	if m.trust.top+m.trustHeight() >= len(m.trustLines(p, max(m.width, 20))) {
		m.trust.seenEnd = true
	}
}

// trustKey handles the trust view.
func (m *Model) trustKey(k tea.KeyMsg) tea.Cmd {
	p, ok := m.shownProject()
	page := m.trustHeight()
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
	m.noteTrustSeen()
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
		// applyProjects shows a new version as it arrives, so this is only a
		// backstop. Trusting it would trust something unread.
		m.showProject(p)
		m.status = "The file changed while you were reading it. Read it again from the top, then press y."
		return nil
	case !m.trust.changedAt.IsZero() && m.now().Sub(m.trust.changedAt) < changeSettle:
		m.status = "The file only just changed. Read the new version, then press y."
		return nil
	case m.tooSmallToTrust():
		m.status = "The pane is too small to show every rule properly. Make it bigger, then read it again."
		return nil
	case !m.trust.seenEnd:
		m.status = "Scroll to the end first (j or space), so every rule has been on screen before you trust it."
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
	matched := false
	for i := range m.projects {
		p := &m.projects[i]
		if p.root != msg.root {
			continue
		}
		if msg.forget {
			p.trusted, p.trustedBefore = false, false
		} else if p.sum == msg.sum {
			p.trusted, p.trustedBefore, matched = true, true, true
		} else {
			p.trustedBefore = true
		}
	}
	switch {
	case msg.forget:
		m.status = "Stopped trusting it. Its rules no longer apply."
	case matched:
		m.status = "Trusted this version. Its rules apply to panes in this repo from the next line."
	default:
		m.status = "Trusted the version you read, but the file has changed again since, so that's ignored too. Read the new one."
	}
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
		notes[i] = escapeUnseen(clean(n))
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
	if ok && len(m.projects) > 1 {
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
	where := fmt.Sprintf("lines %d to %d of %d", min(top+1, end), end, len(lines))
	out = append(out, fit(clean(m.status), w), faint.Render(spread(keys, where, w)))
	return strings.Join(out, "\n")
}

// trustLines is everything the trust view says about p, wrapped to w so no
// part of a rule is cut off.
func (m *Model) trustLines(p project, w int) []string {
	var out []string
	add := func(s string) { out = append(out, wrap(escapeUnseen(clean(s)), w)...) }
	// Styles are added after wrapping, so their escapes don't count as width.
	styled := func(style lipgloss.Style, s string) {
		for _, l := range wrap(escapeUnseen(clean(s)), w) {
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
	// The count comes first, so a rule can't pass for the end of the list.
	add(fmt.Sprintf("It has %s, %s and %s:",
		plural(len(c.Presets), "preset", "presets"),
		plural(len(c.Patterns), "error pattern", "error patterns"),
		plural(len(c.Ignores), "ignore rule", "ignore rules")))
	// Names and rules are shown quoted with everything outside plain ASCII
	// escaped, so where each one ends is plain and no character can make it
	// read as something else. A long one wraps indented under its own line.
	// Every line is wrapped to fit after the deeper indent, because the
	// terminal cuts anything wider and a cut could hide part of a rule.
	rule := func(label, text string) {
		for i, l := range wrap(label+strconv.QuoteToASCII(text), max(w-6, 1)) {
			if i == 0 {
				out = append(out, "  "+l)
			} else {
				out = append(out, "      "+l)
			}
		}
	}
	if len(c.Presets) > 0 {
		var on, off []string
		for _, name := range c.Presets {
			if slices.Contains(m.cfg.Presets, name) {
				on = append(on, name)
			} else {
				off = append(off, name)
			}
		}
		switch {
		case m.cfg.PresetsSet && len(off) > 0:
			add("  presets " + strings.Join(off, ", ") + " (left out: your sightline.toml lists presets, and yours win)")
		case len(off) > 0:
			add("  presets " + strings.Join(off, ", ") + " (switched on for this repo)")
		}
		if len(on) > 0 {
			add("  presets " + strings.Join(on, ", ") + " (already on, so nothing changes)")
		}
	}
	for i, pat := range c.Patterns {
		rule(fmt.Sprintf("error pattern %d of %d, %s: ", i+1, len(c.Patterns), strconv.QuoteToASCII(pat.Name)), pat.Regex.String())
	}
	for i, ig := range c.Ignores {
		rule(fmt.Sprintf("ignore rule %d of %d: ", i+1, len(c.Ignores)), ig.Regex.String())
	}
	add("That's the end of the list.")
	out = append(out, "")
	add("Anyone who can push to this repo can change this file. Only trust it if you've read every rule above.")
	if len(c.Ignores) > 0 {
		out = append(out, "")
		styled(newStyle, "An ignore rule can hide a real error from you, and from the check on whether a fix worked.")
	}
	return out
}

// escapeUnseen writes every character that doesn't show as itself as an escape,
// \x{202e} for a right to left override say. clean only deals with control
// characters, but a rule someone else wrote could use a format character or an
// invisible one to read as something it isn't, and this is where it's read
// before it's trusted.
func escapeUnseen(s string) string {
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
