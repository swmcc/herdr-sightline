package injection

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// The corpus is a set of files in internal/engine/testdata/injection, each one
// a hostile script for one watched pane.
//
// The top of a file says what must hold, one "# key: value" line each:
//
//	# title: what the pane's title is set to (the pane can set its own)
//	# command: what the pane is running
//	# path: raw or herdr to run only one path (see below). Both by default.
//	# hidden: text a terminal hides, which must never reach a message unless
//	#         that entry is flagged MaybeGap
//	# absent: text that must never reach any message at all
//	# caught: text that must reach some message, so the error was seen
//	# shown: text that must reach some message on an entry not flagged MaybeGap
//
// Any other line starting "#" before the script is a comment. Then the script,
// one step per "@" line:
//
//	@ feed        the lines that follow, up to the next "@", are one Feed call
//	@ repeat N x  adds N copies of the line x to the current Feed call, with
//	              {n} in it replaced by the copy's number
//	@ gap         the adapter reports a gap (a reconnect, say)
//	@ wait 2s     the clock moves on and the engine is ticked
//
// Every value and line is unescaped Go style: \e is ESC, \xHH is one raw byte
// (so \x9d is a raw 8-bit C1, not valid UTF-8), \uHHHH and \UHHHHHHHH are
// characters, and \\ is a backslash. {{N|text}} repeats text N times.
//
// Each file runs twice:
//
//   - raw: each Feed call goes straight to engine.Feed, the way a raw stream
//     would, one element per line. This is where escape sequences matter.
//   - herdr: the lines are printed into a fake pane, which a herdr.PaneWatcher
//     reads after each step and diffs, and the adapter's loop hands the result
//     to the engine (Gap for a gap read, Unsure for an ambiguous one). Real
//     herdr strips escapes before Sightline sees them, so here they're a second
//     line of defence being tested.
const corpusDir = "../engine/testdata/injection"

type step struct {
	kind  string // feed, gap or wait
	lines []string
	wait  time.Duration
}

type corpusCase struct {
	name                          string
	title, command                string
	paths                         []string
	hidden, absent, caught, shown []string
	steps                         []step
}

var (
	macro   = regexp.MustCompile(`\{\{(\d+)\|(.*?)\}\}`)
	metaKey = regexp.MustCompile(`^[a-z]+$`) // "# hidden: x" is a key, "# Note: x" a comment
)

// unescape reads one line of the corpus. See the comment on corpusDir.
func unescape(s string) (string, error) {
	var out strings.Builder
	for len(s) > 0 {
		loc := macro.FindStringSubmatchIndex(s)
		end := len(s)
		if loc != nil {
			end = loc[0]
		}
		plain, err := unescapePlain(s[:end])
		if err != nil {
			return "", err
		}
		out.WriteString(plain)
		if loc == nil {
			break
		}
		n, _ := strconv.Atoi(s[loc[2]:loc[3]])
		text, err := unescapePlain(s[loc[4]:loc[5]])
		if err != nil {
			return "", err
		}
		out.WriteString(strings.Repeat(text, n))
		s = s[loc[1]:]
	}
	return out.String(), nil
}

func unescapePlain(s string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out.WriteByte(s[i])
			continue
		}
		if i+1 == len(s) {
			return "", fmt.Errorf("backslash at the end of %q", s)
		}
		i++
		hex := func(n int) (uint64, error) {
			if i+1+n > len(s) {
				return 0, fmt.Errorf("short escape in %q", s)
			}
			v, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
			i += n
			return v, err
		}
		switch c := s[i]; c {
		case '\\':
			out.WriteByte('\\')
		case 'e':
			out.WriteByte(0x1b)
		case 'a':
			out.WriteByte(0x07)
		case 'b':
			out.WriteByte(0x08)
		case 't':
			out.WriteByte('\t')
		case 'r':
			out.WriteByte('\r')
		case 'n':
			out.WriteByte('\n')
		case 'v':
			out.WriteByte('\v')
		case 'f':
			out.WriteByte('\f')
		case '0':
			out.WriteByte(0)
		case 'x':
			v, err := hex(2)
			if err != nil {
				return "", err
			}
			out.WriteByte(byte(v))
		case 'u':
			v, err := hex(4)
			if err != nil {
				return "", err
			}
			out.WriteRune(rune(v))
		case 'U':
			v, err := hex(8)
			if err != nil {
				return "", err
			}
			out.WriteRune(rune(v))
		default:
			return "", fmt.Errorf("unknown escape \\%c in %q", c, s)
		}
	}
	return out.String(), nil
}

func loadCase(t *testing.T, path string) corpusCase {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c := corpusCase{name: strings.TrimSuffix(filepath.Base(path), ".txt"), paths: []string{"raw", "herdr"}}
	must := func(s string) string {
		t.Helper()
		v, err := unescape(s)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return v
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	inScript := false
	for _, l := range lines {
		if !inScript && !strings.HasPrefix(l, "@") {
			key, value, ok := strings.Cut(strings.TrimPrefix(l, "# "), ": ")
			if !strings.HasPrefix(l, "# ") || !ok || !metaKey.MatchString(key) {
				continue
			}
			switch key {
			case "title":
				c.title = must(value)
			case "command":
				c.command = must(value)
			case "path":
				c.paths = []string{value}
			case "hidden":
				c.hidden = append(c.hidden, must(value))
			case "absent":
				c.absent = append(c.absent, must(value))
			case "caught":
				c.caught = append(c.caught, must(value))
			case "shown":
				c.shown = append(c.shown, must(value))
			default:
				// A misspelt key would quietly turn its check off.
				t.Fatalf("%s: unknown key %q", path, key)
			}
			continue
		}
		if strings.HasPrefix(l, "@ ") {
			inScript = true
			fields := strings.SplitN(strings.TrimPrefix(l, "@ "), " ", 3)
			switch fields[0] {
			case "feed":
				c.steps = append(c.steps, step{kind: "feed"})
			case "gap":
				c.steps = append(c.steps, step{kind: "gap"})
			case "wait":
				if len(fields) < 2 {
					t.Fatalf("%s: wait needs a duration", path)
				}
				d, err := time.ParseDuration(fields[1])
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				c.steps = append(c.steps, step{kind: "wait", wait: d})
			case "repeat":
				if len(fields) < 3 {
					t.Fatalf("%s: bad repeat %q", path, l)
				}
				n, err := strconv.Atoi(fields[1])
				if err != nil || len(c.steps) == 0 || c.steps[len(c.steps)-1].kind != "feed" {
					t.Fatalf("%s: bad repeat %q", path, l)
				}
				st := &c.steps[len(c.steps)-1]
				for i := range n {
					st.lines = append(st.lines, must(strings.ReplaceAll(fields[2], "{n}", strconv.Itoa(i))))
				}
			default:
				t.Fatalf("%s: unknown step %q", path, l)
			}
			continue
		}
		if len(c.steps) == 0 || c.steps[len(c.steps)-1].kind != "feed" {
			t.Fatalf("%s: line %q outside a feed", path, l)
		}
		st := &c.steps[len(c.steps)-1]
		st.lines = append(st.lines, must(l))
	}
	if len(c.hidden)+len(c.absent)+len(c.caught)+len(c.shown) == 0 {
		t.Fatalf("%s: no hidden, absent, caught or shown, so only the format is checked", path)
	}
	if len(c.steps) == 0 {
		t.Fatalf("%s: no script", path)
	}
	return c
}

const paneID = "w1:p3"

// options are what every corpus case runs with: every preset plus a catch all
// for "Error" anywhere, so detection is as eager as it gets.
func options(t testing.TB) engine.Options {
	t.Helper()
	presets, err := engine.Presets(engine.PresetNames()...)
	if err != nil {
		t.Fatal(err)
	}
	return engine.Options{
		Matchers:     append(presets, engine.Regexp("catch-all", regexp.MustCompile(`Error`))),
		ContextLines: 10,
		CaptureWait:  time.Second,
		Settle:       30 * time.Second,
	}
}

var start = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

// runRaw plays c straight into the engine.
func runRaw(t *testing.T, c corpusCase) *engine.Engine {
	e := engine.New(options(t))
	e.Watch(engine.Source{ID: paneID, Title: c.title, Command: c.command})
	now := start
	for _, s := range c.steps {
		now = now.Add(10 * time.Millisecond)
		switch s.kind {
		case "feed":
			e.Feed(paneID, s.lines, now)
		case "gap":
			e.Gap(paneID, now)
		case "wait":
			now = now.Add(s.wait)
			e.Tick(now)
		}
	}
	e.Tick(now.Add(time.Minute))
	return e
}

// fakePane is a pane's history, read the way herdr's pane.read returns it: the
// last lines joined with new lines, with the empty line the cursor sits on last.
type fakePane struct {
	lines []string
}

func (p *fakePane) ReadPane(_ context.Context, id string, n int) (herdr.PaneRead, error) {
	from := max(0, len(p.lines)-n+1)
	return herdr.PaneRead{
		PaneID:    id,
		Text:      strings.Join(append(append([]string(nil), p.lines[from:]...), ""), "\n"),
		Truncated: from > 0,
	}, nil
}

// adapterPoll is the adapter's loop body: read, then tell the engine what the
// read could and couldn't say, then hand it the lines.
func adapterPoll(t testing.TB, w *herdr.PaneWatcher, e *engine.Engine, now time.Time) {
	t.Helper()
	d, err := w.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Gap {
		e.Gap(paneID, now)
	} else if d.Ambiguous {
		e.Unsure(paneID, now)
	}
	if len(d.Lines) > 0 {
		e.Feed(paneID, d.Lines, now)
	}
	e.Tick(now)
}

// runHerdr plays c into a fake pane and runs it through the watcher.
func runHerdr(t *testing.T, c corpusCase) *engine.Engine {
	e := engine.New(options(t))
	e.Watch(engine.Source{ID: paneID, Title: c.title, Command: c.command})
	pane := &fakePane{}
	w := herdr.NewPaneWatcher(pane, paneID)
	now := start
	adapterPoll(t, w, e, now)
	for _, s := range c.steps {
		now = now.Add(10 * time.Millisecond)
		switch s.kind {
		case "feed":
			for _, l := range s.lines {
				pane.lines = append(pane.lines, strings.Split(l, "\n")...)
			}
			adapterPoll(t, w, e, now)
		case "gap":
			e.Gap(paneID, now)
		case "wait":
			now = now.Add(s.wait)
			adapterPoll(t, w, e, now)
		}
	}
	e.Tick(now.Add(time.Minute))
	return e
}

func TestCorpus(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 10 {
		t.Fatalf("found %d corpus files in %s", len(files), corpusDir)
	}
	for _, f := range files {
		c := loadCase(t, f)
		for _, path := range c.paths {
			t.Run(c.name+"/"+path, func(t *testing.T) {
				var e *engine.Engine
				switch path {
				case "raw":
					e = runRaw(t, c)
				case "herdr":
					e = runHerdr(t, c)
				default:
					t.Fatalf("unknown path %q", path)
				}
				checkEngine(t, e, c.hidden, c.absent, c.caught, c.shown)
			})
		}
	}
}

// checkEngine reads every entry's message with the parser and checks the
// case's promises.
func checkEngine(t *testing.T, e *engine.Engine, hidden, absent, caught, shown []string) {
	t.Helper()
	entries := e.Entries()
	seen := make(map[string]bool)
	for _, en := range entries {
		msg, err := e.Message(en.ID)
		if err != nil {
			t.Fatal(err)
		}
		parse(t, msg, en)
		for _, h := range hidden {
			if strings.Contains(msg, h) && !en.MaybeGap {
				t.Errorf("text a terminal hides (%q) reached entry %s, which isn't flagged MaybeGap:\n%s", h, en.ID, quoteLines(msg))
			}
		}
		for _, a := range absent {
			if strings.Contains(msg, a) {
				t.Errorf("%q reached entry %s:\n%s", a, en.ID, quoteLines(msg))
			}
		}
		for _, c := range caught {
			if strings.Contains(msg, c) {
				seen["caught:"+c] = true
			}
		}
		for _, s := range shown {
			if strings.Contains(msg, s) && !en.MaybeGap {
				seen["shown:"+s] = true
			}
		}
	}
	for _, c := range caught {
		if !seen["caught:"+c] {
			t.Errorf("no message has %q, so the error wasn't caught (%d entries)", c, len(entries))
		}
	}
	for _, s := range shown {
		if !seen["shown:"+s] {
			t.Errorf("no unflagged message has %q (%d entries)", s, len(entries))
		}
	}
}
