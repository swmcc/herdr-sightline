package feed

// Never again (#14). Pressing n on an entry suggests an ignore rule for its
// error line, lets the person change it and, only when they press enter, adds it
// to the end of sightline.toml. The feed doesn't apply the rule itself. The
// config watch sees the file change and the reload applies it through
// engine.Reconfigure like any other edit, so there's one way a rule takes effect.
//
// The suggestion comes from pane output, which is untrusted, so it's shown in
// full and nothing is written without the keypress. config.AppendIgnore refuses
// anything that wouldn't load.

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/swmcc/herdr-sightline/internal/config"
)

// undoFor is how long u can take a new rule back out. After that the rule is
// just part of the file.
const undoFor = 30 * time.Second

// maxRule is the longest rule the feed will save. A rule is meant to be read.
const maxRule = 2000

// suggestFrom is how much of a long line the suggestion uses. Past this the rule
// is a prefix of the line rather than all of it.
const suggestFrom = 160

// neverState is the rule being written and the last one added.
type neverState struct {
	line   string // the entry's error line the rule is for
	text   []rune // the rule as typed
	cursor int    // where in text the next rune goes
	busy   bool   // a save or undo is running

	added   *config.Appended // the last rule added, until undone or too old
	addedAt time.Time
	applied bool   // a reload has picked the added rule up
	removed string // a rule just undone, until a reload shows it gone
}

// neverMsg is the result of saving or undoing a rule.
type neverMsg struct {
	undo     bool
	appended config.Appended
	err      error
}

// healthy is ordinary output a rule must never match. A rule that does would
// hide far more than the entry it was written for.
var healthy = []string{
	"", " ", "a", "0", "ok", "{}",
	"GET / 200",
	"ready in 312 ms",
	"compiled successfully",
	"Listening on http://localhost:3000",
	"INFO server started",
	"Tests: 4 passed",
}

// checkRule says whether expr is fit to save as a rule for line. It's shown
// live under the rule, so a problem is said before enter is pressed.
func checkRule(expr, line string) (ok bool, note string) {
	if expr == "" {
		return false, "Type a regex."
	}
	if len(expr) > maxRule {
		return false, fmt.Sprintf("That's too long for a rule (over %d bytes).", maxRule)
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return false, fmt.Sprintf("Doesn't compile: %v", err)
	}
	if re.MatchString("") {
		return false, "It matches an empty line, so it would hide everything."
	}
	for _, h := range healthy[1:] {
		if re.MatchString(h) {
			return false, fmt.Sprintf("It also matches ordinary output like %q, so it would hide far too much.", h)
		}
	}
	if !re.MatchString(line) {
		return false, "It doesn't match this entry's line."
	}
	for _, p := range otherErrors(line) {
		if re.MatchString(p) {
			return false, fmt.Sprintf("It also matches other errors like %q, so it would hide far too much.", p)
		}
	}
	return true, "Compiles and matches this entry's line."
}

// otherErrors are lines a rule for line must not match: common errors, and
// line's own start followed by something unrelated. A rule that matches those
// hides a whole kind of error rather than this one. The pane chooses the line,
// so without this it could steer the suggestion to ^Error: and still pass.
func otherErrors(line string) []string {
	probes := []string{"Error: boom", "panic: boom", "TypeError: boom", "error: boom"}
	start := ""
	if i := strings.Index(line, ": "); i >= 0 {
		start = line[:i+2]
	} else if i := strings.IndexByte(strings.TrimLeftFunc(line, unicode.IsSpace), ' '); i >= 0 {
		lead := len(line) - len(strings.TrimLeftFunc(line, unicode.IsSpace))
		start = line[:lead+i+1]
	}
	if start != "" {
		for _, rest := range []string{"zzq", "zzq unrelated words here", "'zzq unrelated'", `"zzq unrelated"`, "`zzq unrelated'"} {
			probes = append(probes, start+rest)
		}
		// The same shape with every word swapped, which catches a rule that
		// made every word after the start general.
		probes = append(probes, start+anyWord.ReplaceAllString(line[len(start):], "zzq"))
	}
	return slices.DeleteFunc(probes, func(p string) bool { return p == line })
}

var (
	// cutMarker is what the engine adds to a line it shortened to store.
	cutMarker = regexp.MustCompile(` \[\d+ more bytes cut\]$`)
	anyWord   = regexp.MustCompile(`\S+`)
	uuid      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	hexNumber = regexp.MustCompile(`^0[xX][0-9a-fA-F]+`)
	hexRun    = regexp.MustCompile(`^[0-9a-fA-F]{7,}`)
)

// suggestRule turns an error line into a rule that matches it and its repeats.
// The parts that change from one run to the next (numbers, ids, hex, quoted
// values and, after the first ": ", paths) become patterns. Everything else is
// kept literally. It's anchored at the start, and at the end too unless the line
// was long and only its start is used.
//
// If that would hide other errors as well (see checkRule), it falls back to the
// line exactly as printed. A narrow rule is better than a broad one.
func suggestRule(line string) string {
	s := buildRule(line, true)
	if fit, _ := checkRule(s, line); fit {
		return s
	}
	return buildRule(line, false)
}

// reSpace is what RE2's \s matches. Only this is trimmed and written as \s*, so
// a line padded with something wider, like a non-breaking space, keeps it as a
// literal and the rule still matches.
const reSpace = " \t\n\f\r"

// buildRule writes the rule for line, with the parts that change made general
// when general is set and everything literal when it isn't.
func buildRule(line string, general bool) string {
	open := false
	// Only a line long enough to have been cut really was. A pane can print the
	// marker itself to drop the end anchor from a short line.
	if loc := cutMarker.FindStringIndex(line); loc != nil && loc[0] >= suggestFrom {
		line, open = line[:loc[0]], true
	}
	body := strings.TrimRight(line, reSpace)
	trailing := len(body) < len(line)
	trimmed := strings.TrimLeft(body, reSpace)
	leading := len(trimmed) < len(body)
	body = trimmed
	if len(body) > suggestFrom {
		// Cut at a space when there's one far enough in. One near the start
		// would leave a rule of a word or two.
		cut := strings.LastIndexByte(body[:suggestFrom], ' ')
		if cut < suggestFrom/2 {
			cut = suggestFrom
			for cut > 0 && !utf8.RuneStart(body[cut]) {
				cut--
			}
		}
		body, open = body[:cut], true
	}

	var b, lit strings.Builder
	flush := func() {
		b.WriteString(quoteLiteral(lit.String()))
		lit.Reset()
	}
	pattern := func(p string) {
		flush()
		b.WriteString(p)
	}
	b.WriteString("^")
	if leading {
		b.WriteString(`\s*`)
	}
	pastColon := false
	for i := 0; i < len(body); {
		rest := body[i:]
		prev := byte(' ')
		if i > 0 {
			prev = body[i-1]
		}
		atWord := !isWord(prev)
		c := body[i]

		// A quoted value. An apostrophe inside a word ("can't") isn't a quote.
		if general && (c == '\'' || c == '"' || c == '`') && atWord {
			closer := c
			end := strings.IndexByte(rest[1:], closer)
			if c == '`' {
				// Ruby quotes `like this'.
				if alt := strings.IndexByte(rest[1:], '\''); alt >= 0 && (end < 0 || alt < end) {
					end, closer = alt, '\''
				}
			}
			if end >= 0 {
				pattern(regexp.QuoteMeta(string(c)) + `[^` + regexp.QuoteMeta(string(closer)) + `]*` + regexp.QuoteMeta(string(closer)))
				i += end + 2
				continue
			}
		}
		if general && atWord {
			if m := uuid.FindString(rest); m != "" && wordEnds(rest, len(m)) {
				pattern(`[0-9a-fA-F-]+`)
				i += len(m)
				continue
			}
			if m := hexNumber.FindString(rest); m != "" && wordEnds(rest, len(m)) {
				pattern(`0[xX][0-9a-fA-F]+`)
				i += len(m)
				continue
			}
			if m := hexRun.FindString(rest); m != "" && wordEnds(rest, len(m)) &&
				strings.ContainsAny(m, "0123456789") && strings.ContainsAny(m, "abcdefABCDEF") {
				pattern(`[0-9a-fA-F]+`)
				i += len(m)
				continue
			}
			// A path after the first ": " is where it happened, which moves.
			if pastColon && prev == ' ' {
				token := rest
				// Split where \S stops, so \S+ covers the whole token.
				if j := strings.IndexAny(rest, reSpace); j >= 0 {
					token = rest[:j]
				}
				if strings.ContainsAny(token, `/\`) {
					pattern(`\S+`)
					i += len(token)
					continue
				}
			}
		}
		// A number, unless it's part of a name like E0425 or utf8.
		if general && isDigit(c) && !wordStartsWithLetter(body, i) {
			j := i
			for j < len(body) && isDigit(body[j]) {
				j++
			}
			pattern(`\d+`)
			i = j
			continue
		}
		if c == ':' && i+1 < len(body) && body[i+1] == ' ' {
			pastColon = true
		}
		_, size := utf8.DecodeRuneInString(rest)
		lit.WriteString(rest[:size])
		i += size
	}
	flush()
	if !open {
		if trailing {
			b.WriteString(`\s*`)
		}
		b.WriteString("$")
	}
	return b.String()
}

// quoteLiteral escapes s for a regex, writing anything that isn't printable as
// an escape so the rule is all visible text.
func quoteLiteral(s string) string {
	var b strings.Builder
	for _, r := range regexp.QuoteMeta(s) {
		switch {
		case r == '\t':
			b.WriteString(`\t`)
		case r == utf8.RuneError || !unicode.IsPrint(r) && r != ' ':
			fmt.Fprintf(&b, `\x{%x}`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isWord(c byte) bool {
	return isDigit(c) || c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z')
}

// wordEnds reports whether a match of n bytes at the start of s ends a word.
func wordEnds(s string, n int) bool { return n >= len(s) || !isWord(s[n]) }

// wordStartsWithLetter reports whether the word around s[i] starts with a
// letter, which makes its digits part of a name.
func wordStartsWithLetter(s string, i int) bool {
	for i > 0 && isWord(s[i-1]) {
		i--
	}
	return !isDigit(s[i])
}

// openNever starts writing a rule for an entry, with a suggestion to edit.
func (m *Model) openNever(lines []string) {
	if m.configDir == "" {
		m.status = "There's no config directory to save a rule in (HERDR_PLUGIN_CONFIG_DIR isn't set)."
		return
	}
	if len(lines) == 0 {
		return
	}
	m.never.line = lines[0]
	m.never.text = []rune(suggestRule(lines[0]))
	m.never.cursor = len(m.never.text)
	m.mode = modeNever
	m.status = ""
}

// neverKey edits the rule. Every printable key types, so q and the rest don't
// do what they do in the list.
func (m *Model) neverKey(k tea.KeyMsg) tea.Cmd {
	n := &m.never
	if n.busy {
		// The save's result decides what happens next. Leaving now would say
		// no rule was added when one may have been.
		m.status = "Still saving the rule."
		return nil
	}
	switch k.Type {
	case tea.KeyEsc:
		m.mode = modeList
		m.status = "No rule added."
	case tea.KeyEnter:
		return m.saveRule()
	case tea.KeyLeft, tea.KeyCtrlB:
		n.cursor = max(n.cursor-1, 0)
	case tea.KeyRight, tea.KeyCtrlF:
		n.cursor = min(n.cursor+1, len(n.text))
	case tea.KeyHome, tea.KeyCtrlA:
		n.cursor = 0
	case tea.KeyEnd, tea.KeyCtrlE:
		n.cursor = len(n.text)
	case tea.KeyBackspace, tea.KeyCtrlH:
		if n.cursor > 0 {
			n.text = slices.Delete(n.text, n.cursor-1, n.cursor)
			n.cursor--
		}
	case tea.KeyDelete, tea.KeyCtrlD:
		if n.cursor < len(n.text) {
			n.text = slices.Delete(n.text, n.cursor, n.cursor+1)
		}
	case tea.KeyCtrlU:
		n.text, n.cursor = slices.Delete(n.text, 0, n.cursor), 0
	case tea.KeyCtrlK:
		n.text = n.text[:n.cursor]
	case tea.KeyCtrlW:
		start := n.cursor
		for start > 0 && n.text[start-1] == ' ' {
			start--
		}
		for start > 0 && n.text[start-1] != ' ' {
			start--
		}
		n.text, n.cursor = slices.Delete(n.text, start, n.cursor), start
	case tea.KeySpace:
		n.insert([]rune{' '})
	case tea.KeyRunes:
		n.insert(k.Runes)
	}
	return nil
}

// insert types runes at the cursor. A paste can carry line breaks and other
// control characters, which a one line rule can't hold, so they're dropped.
func (n *neverState) insert(rs []rune) {
	rs = slices.DeleteFunc(slices.Clone(rs), func(r rune) bool { return unicode.IsControl(r) || r == utf8.RuneError })
	n.text = slices.Insert(n.text, n.cursor, rs...)
	n.cursor += len(rs)
}

// saveRule writes the rule once enter is pressed, if it passes the checks.
func (m *Model) saveRule() tea.Cmd {
	n := &m.never
	if n.busy {
		m.status = "Still working on the last rule."
		return nil
	}
	expr := string(n.text)
	if ok, note := checkRule(expr, n.line); !ok {
		m.status = "Not saved. " + note
		return nil
	}
	n.busy = true
	m.status = "Saving the rule..."
	dir, now := m.configDir, m.now()
	return func() tea.Msg {
		a, err := config.AppendIgnore(dir, expr, now)
		return neverMsg{appended: a, err: err}
	}
}

// undoRule takes the last rule back out, if it's recent enough.
func (m *Model) undoRule() tea.Cmd {
	n := &m.never
	switch {
	case n.added == nil:
		return nil
	case n.busy:
		m.status = "Still working on the last rule."
		return nil
	case m.now().Sub(n.addedAt) > undoFor:
		m.status = fmt.Sprintf("Too late to undo from the feed. The rule is the last [[ignore]] in %s.", n.added.Path)
		n.added = nil
		return nil
	}
	n.busy = true
	m.status = "Removing the rule..."
	a := *n.added
	return func() tea.Msg {
		return neverMsg{undo: true, appended: a, err: config.RemoveAppended(a)}
	}
}

// neverDone takes the result of a save or an undo.
func (m *Model) neverDone(msg neverMsg) {
	n := &m.never
	n.busy = false
	switch {
	case msg.undo && errors.Is(msg.err, config.ErrChanged):
		m.status = fmt.Sprintf("Can't undo safely: sightline.toml has changed since the rule was added. Remove it by hand from %s.", msg.appended.Path)
		n.added = nil
	case msg.undo && msg.err != nil:
		m.status = fmt.Sprintf("Couldn't undo the rule: %v", msg.err)
	case msg.undo:
		m.status = "Removed the rule, so sightline.toml is as it was. Anything it dismissed stays dismissed."
		n.removed = n.added.Regex
		n.added = nil
	case msg.err != nil:
		// Stay in the editor so the rule isn't lost.
		m.status = fmt.Sprintf("Not saved: %v", msg.err)
	default:
		a := msg.appended
		n.added, n.addedAt, n.applied, n.removed = &a, m.now(), false, ""
		m.mode = modeList
		m.status = fmt.Sprintf("Added an ignore rule to sightline.toml. It applies once the feed reloads the file. Press u within %ds to undo it.", int(undoFor.Seconds()))
	}
}

// noteRule follows a reload. The first reload that has the new rule in it is
// the one that applied it, so the status says the rule is in and how to undo
// it, rather than just that the file was reloaded.
//
// The same goes for an undo: the reload after it says the rule is gone.
func (m *Model) noteRule() {
	n := &m.never
	if m.cfgErr != nil {
		return
	}
	if n.removed != "" && !slices.ContainsFunc(m.cfg.Ignores, func(ig config.Ignore) bool { return ig.Regex.String() == n.removed }) {
		n.removed = ""
		m.status = "Rule removed. " + m.status
	}
	if n.added == nil || n.applied {
		return
	}
	for _, ig := range m.cfg.Ignores {
		if ig.Regex.String() == n.added.Regex {
			n.applied = true
			m.status = "Ignore rule added. " + m.status
			if left := undoFor - m.now().Sub(n.addedAt); left > 0 {
				m.status += fmt.Sprintf(" Press u within %ds to undo the rule.", int(left.Seconds()+0.5))
			}
			return
		}
	}
}

// neverView shows the line, the rule being written and whether it's fit to
// save. The rule came from pane output, so all of it is shown before enter.
func (m *Model) neverView() string {
	w := max(m.width, 20)
	n := &m.never
	out := []string{
		faint.Render(fit("Never again: an ignore rule for this entry", w)),
		"",
		faint.Render(fit("The line:", w)),
		fit("  "+clean(n.line), w),
		"",
		faint.Render(fit("Rule (Go RE2 regex, edit it as you like):", w)),
	}
	out = append(out, ruleLines(n.text, n.cursor, w)...)
	out = append(out, "")
	// The note can quote the rule, which can hold anything that was pasted.
	if ok, note := checkRule(string(n.text), n.line); ok {
		out = append(out, okStyle.Render(fit("✓ "+clean(note), w)))
	} else {
		out = append(out, failStyle.Render(fit("✗ "+clean(note), w)))
	}
	out = append(out, faint.Render(fit("Enter adds it to the end of "+config.Path(m.configDir)+".", w)))
	for len(out) < m.height-2 {
		out = append(out, "")
	}
	out = append(out, fit(clean(m.status), w), faint.Render(fit("enter save  esc cancel  ←/→ move  ctrl+u clear", w)))
	return strings.Join(out, "\n")
}

// ruleLines draws the rule wrapped to the width, so every character of it is on
// screen, with the cursor shown in reverse.
func ruleLines(text []rune, cursor, w int) []string {
	w = max(w-2, 8)
	cells := append(slices.Clone(text), ' ') // room for the cursor at the end
	// Rows are measured in cells, not runes, because the terminal cuts anything
	// past its width and a wide character takes two.
	var out []string
	var b strings.Builder
	used := 0
	for i, r := range cells {
		s := clean(string(r))
		width := runewidth.StringWidth(s)
		if used > 0 && used+width > w {
			out = append(out, "  "+b.String())
			b.Reset()
			used = 0
		}
		if i == cursor {
			s = cursorStyle.Render(s)
		}
		b.WriteString(s)
		used += width
	}
	return append(out, "  "+b.String())
}

var cursorStyle = lipgloss.NewStyle().Reverse(true)
