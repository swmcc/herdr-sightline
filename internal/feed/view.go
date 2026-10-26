package feed

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// Colours are the terminal's own first eight, so the feed follows whatever
// theme the terminal has. Nothing blinks and only a fix that didn't work is red.
var (
	faint     = lipgloss.NewStyle().Faint(true)
	bold      = lipgloss.NewStyle().Bold(true)
	newStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	sentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	okStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	failStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// linesShown is how many of an entry's error lines the list shows.
const linesShown = 2

// clean makes text from a pane or from herdr safe to put on the feed's own
// terminal. The engine already strips escapes from output, but titles, labels
// and errors come from elsewhere. Control characters are shown as \x1b and so
// on rather than acted on.
//
// Bytes that aren't valid UTF-8 are shown the same way, since a lone 0x9b is
// an escape to some terminals.
func clean(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unicode.IsControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// tabWidth is where tab stops fall when the view draws a tab.
const tabWidth = 8

// expandTabs draws tabs as spaces up to the next tab stop, the way a terminal
// would. Shown as \x09 a tab would look just like a pane that printed those
// four characters, which isn't what the agent gets.
func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := tabWidth - col%tabWidth
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col += runewidth.RuneWidth(r)
	}
	return b.String()
}

// fit cuts s to width cells.
func fit(s string, width int) string {
	return runewidth.Truncate(s, max(width, 1), "…")
}

// spread puts left and right on one line of width cells, cutting left if it has to.
func spread(left, right string, width int) string {
	room := width - runewidth.StringWidth(right) - 1
	if room < 8 {
		return fit(left, width)
	}
	left = fit(left, room)
	return left + strings.Repeat(" ", width-runewidth.StringWidth(left)-runewidth.StringWidth(right)) + right
}

// ago says how long ago t was, roughly.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

// View draws the feed.
func (m *Model) View() string {
	switch m.mode {
	case modeView:
		return m.messageView()
	case modePick:
		return m.pickView()
	case modeNever:
		return m.neverView()
	}
	return m.listView()
}

func (m *Model) listView() string {
	w := max(m.width, 20)
	top := m.headerLines(w)
	bottom := m.footerLines(w)
	room := max(m.height-len(top)-len(bottom), 1)

	var blocks [][]string
	for i, en := range m.entries {
		blocks = append(blocks, m.entryLines(en, en.ID == m.selected, w))
		if i < len(m.entries)-1 {
			blocks[i] = append(blocks[i], "")
		}
	}
	// Show the selected entry, and as many after it as fit, starting far enough
	// back that it's on screen.
	start := 0
	if c := m.cursor(); c > 0 {
		used := 0
		for i := c; i >= 0; i-- {
			used += len(blocks[i])
			if used > room {
				break
			}
			start = i
		}
	}
	var body []string
	for _, b := range blocks[start:] {
		body = append(body, b...)
	}
	if len(body) > room {
		body = body[:room]
	}
	if len(m.entries) == 0 {
		body = append(body, faint.Render(fit("No errors yet. They'll appear here as they're printed.", w)))
	}
	for len(body) < room {
		body = append(body, "")
	}

	out := append(top, body...)
	out = append(out, bottom...)
	return strings.Join(out, "\n")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// headerLines says what's being watched and anything that's wrong.
func (m *Model) headerLines(w int) []string {
	var names []string
	for _, pane := range m.watching {
		names = append(names, m.paneName(pane))
	}
	head := "Sightline · not watching any panes yet. Run \"Sightline: watch this pane\" on one."
	if len(names) > 0 {
		head = "Sightline · watching " + strings.Join(names, ", ")
	}
	lines := []string{faint.Render(fit(head, w))}
	// Shown up here so a flood of errors can't push it out of sight too.
	if n := m.engine.Dropped(); n > 0 {
		lines = append(lines, faint.Render(fit(fmt.Sprintf("%s dropped to keep the feed short", plural(n, "older entry", "older entries")), w)))
	}

	var problems []string
	if m.herdrErr != nil {
		problems = append(problems, fmt.Sprintf("Can't reach herdr: %v", m.herdrErr))
	}
	if m.eventsErr != nil {
		problems = append(problems, fmt.Sprintf("Not hearing herdr's %v", m.eventsErr))
	}
	if m.cfgErr != nil {
		problems = append(problems, fmt.Sprintf("sightline.toml: %v", m.cfgErr))
	}
	if m.listErr != nil {
		problems = append(problems, m.listErr.Error())
	}
	for _, pane := range m.watching {
		if err := m.paneErrs[pane]; err != nil {
			problems = append(problems, fmt.Sprintf("Can't read %s: %v", m.paneName(pane), err))
		}
	}
	for _, p := range problems {
		lines = append(lines, newStyle.Render(fit(clean(p), w)))
	}
	return append(lines, "")
}

// footerLines is the status line and the keys.
func (m *Model) footerLines(w int) []string {
	return []string{
		"",
		fit(clean(m.status), w),
		faint.Render(fit("j/k move  enter send  v view  t agent  d dismiss  n never  q quit", w)),
	}
}

// marker is the symbol and style for an entry's state.
func marker(s engine.State) (string, lipgloss.Style) {
	switch s {
	case engine.StateSent:
		return "→", sentStyle
	case engine.StateFixed:
		return "✓", okStyle
	case engine.StateStillFailing:
		return "✗", failStyle
	case engine.StateUnverified:
		return "?", faint
	}
	return "●", newStyle
}

// entryLines draws one entry: where it came from, its first lines, who it goes
// to and, when it's selected, the keys.
func (m *Model) entryLines(en engine.Entry, selected bool, w int) []string {
	mark, style := marker(en.State)
	where := clean(en.Source.Title)
	if where == "" {
		where = string(en.Source.ID)
	}
	if en.Source.Command != "" {
		where += " · " + clean(en.Source.Command)
	}
	if en.Count > 1 {
		where += fmt.Sprintf(" ×%d", en.Count)
	}
	head := spread(mark+" "+where, ago(en.LastSeen, m.now()), w)
	if selected {
		head = bold.Render(head)
	}
	lines := []string{style.Render(head)}

	// The error's own lines first, then the output right after it, which for
	// a stack trace is usually where it happened.
	shown := append(slices.Clip(en.Lines), en.After...)
	for i, l := range shown {
		if i == linesShown {
			break
		}
		lines = append(lines, fit("  "+clean(l), w))
	}

	if en.MaybeGap {
		lines = append(lines, faint.Render(fit("  some output around it may have been missed", w)))
	}

	who, keys := m.entryTarget(en)
	if selected && keys != "" {
		lines = append(lines, spread("  "+who, keys, w))
	} else {
		lines = append(lines, faint.Render(fit("  "+who, w)))
	}
	return lines
}

// entryTarget says who an entry goes to, or what happened to it, and the keys
// that make sense for it.
func (m *Model) entryTarget(en engine.Entry) (who, keys string) {
	const sendKeys = "[enter] send   [d] dismiss   [n] never"
	agent := clean(m.agentLabel(en.Agent))
	switch en.State {
	case engine.StateSent:
		if m.sending[en.ID] {
			return "sending again…", ""
		}
		return "sent to " + agent + ", waiting for it to finish", "[v] view   [d] dismiss"
	case engine.StateFixed:
		return "✓ fixed, it stopped appearing after " + agent + " finished", "[d] dismiss"
	case engine.StateUnverified:
		if en.Note != "" {
			return "? unverified, " + clean(en.Note) + ". Send it again or check it yourself", "[enter] send again   [d] dismiss"
		}
		return "? unverified, nothing ran to prove it either way. Check it yourself", "[enter] send again   [d] dismiss"
	}
	if m.sending[en.ID] {
		return "sending…", ""
	}
	t, ask := m.targetFor(en)
	prefix := ""
	if en.State == engine.StateStillFailing {
		prefix = "✗ still failing after " + agent + " finished. "
		keys = "[enter] send again   [d] dismiss   [n] never"
	} else {
		keys = sendKeys
	}
	if ask != "" {
		return prefix + "? asking: " + ask, "[t] pick agent   [d] dismiss"
	}
	who = prefix + "→ " + clean(t.Label) + m.pinNote(en)
	if t.Status == herdr.AgentBlocked {
		who += " (waiting on a prompt, won't send)"
	}
	return who, keys
}

// viewHeight is how many lines of the message fit on screen.
func (m *Model) viewHeight() int { return max(m.height-4, 1) }

// viewLines is the message as it's drawn: every byte, with long lines wrapped
// rather than cut, so nothing that will be sent is hidden.
func (m *Model) viewLines() []string {
	w := max(m.width, 20)
	var out []string
	for _, l := range strings.Split(m.viewText, "\n") {
		l = clean(expandTabs(l))
		for runewidth.StringWidth(l) > w {
			part := runewidth.Truncate(l, w, "")
			if part == "" {
				break
			}
			out = append(out, part)
			l = l[len(part):]
		}
		out = append(out, l)
	}
	return out
}

func (m *Model) messageView() string {
	w := max(m.width, 20)
	lines := m.viewLines()
	h := m.viewHeight()
	top := min(m.viewTop, max(len(lines)-h, 0))
	end := min(top+h, len(lines))

	head := fmt.Sprintf("Exactly what enter sends · %s", plural(len(m.viewText), "byte", "bytes"))
	// The agent named here is the one enter checks against, so the header
	// can't say one agent while enter sends to another.
	if m.viewTarget.PaneID != "" {
		head = fmt.Sprintf("Exactly what enter sends to %s · %s", clean(m.viewTarget.Label), plural(len(m.viewText), "byte", "bytes"))
	}
	out := []string{faint.Render(fit(head, w)), ""}
	out = append(out, lines[top:end]...)
	for len(out) < h+2 {
		out = append(out, "")
	}
	where := fmt.Sprintf("lines %d to %d of %d", top+1, end, len(lines))
	out = append(out, fit(clean(m.status), w), faint.Render(spread("j/k scroll  enter send  esc back", where, w)))
	return strings.Join(out, "\n")
}

func (m *Model) pickView() string {
	w := max(m.width, 20)
	out := []string{faint.Render(fit("Pick the agent for this entry", w)), ""}
	for i, t := range m.pickList {
		line := clean(t.Label)
		if t.Status != "" {
			line += " · " + string(t.Status)
		}
		if t.Status == herdr.AgentBlocked {
			line += " (waiting on a prompt)"
		}
		if !t.SameTab {
			line += " · another tab"
		}
		if i == m.pickCursor {
			out = append(out, bold.Render(fit("› "+line, w)))
		} else {
			out = append(out, fit("  "+line, w))
		}
	}
	out = append(out, "", faint.Render(fit("j/k move  enter choose  esc back", w)))
	return strings.Join(out, "\n")
}
