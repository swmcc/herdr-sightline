package engine

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Stand-in until #8. A fixed size window per source, the matched line as the
// error and up to ContextLines either side. #8 decides how much of a stack trace
// counts as the error itself.

// window keeps the last few lines from a source, for the context before a match.
type window struct {
	size  int
	lines []string
}

func (w *window) push(line string) {
	if w.size <= 0 {
		return
	}
	w.lines = append(w.lines, line)
	if len(w.lines) > w.size {
		w.lines = w.lines[len(w.lines)-w.size:]
	}
}

func (w *window) snapshot() []string {
	return append([]string(nil), w.lines...)
}

func (w *window) reset() {
	w.lines = nil
}

// capture is an error that has matched but is still collecting the lines after it.
type capture struct {
	lines     []string
	before    []string
	after     []string
	matchedAt time.Time
	due       time.Time
	maybeGap  bool
	seq       int // order within a source, for captures that matched at the same time
}

// observe takes one clean line from s.
func (e *Engine) observe(s *source, line string, now time.Time) {
	for _, c := range s.pending {
		if len(c.after) < e.opts.ContextLines {
			c.after = append(c.after, line)
		}
	}
	if e.startsError(line) {
		s.pending = append(s.pending, &capture{
			lines:     []string{line},
			before:    s.window.snapshot(),
			matchedAt: now,
			due:       now.Add(e.opts.CaptureWait),
			seq:       s.captured,
		})
		s.captured++
	}
	s.window.push(line)
}

// finishCaptures turns every capture whose wait is over into an entry. They're
// filed in the order they matched, across all sources, so the feed's order never
// depends on map iteration.
func (e *Engine) finishCaptures(now time.Time) {
	type finished struct {
		src Source
		c   *capture
	}
	var due []finished
	for _, s := range e.sources {
		kept := s.pending[:0]
		for _, c := range s.pending {
			if now.Before(c.due) {
				kept = append(kept, c)
				continue
			}
			due = append(due, finished{s.info, c})
		}
		clear(s.pending[len(kept):])
		s.pending = kept
	}
	sort.Slice(due, func(i, j int) bool {
		a, b := due[i], due[j]
		if !a.c.matchedAt.Equal(b.c.matchedAt) {
			return a.c.matchedAt.Before(b.c.matchedAt)
		}
		if a.src.ID != b.src.ID {
			return a.src.ID < b.src.ID
		}
		return a.c.seq < b.c.seq
	})
	for _, f := range due {
		e.group(f.src, f.c)
	}
}

// splitLines breaks any embedded newlines out and cleans every line, so the rest
// of the engine only ever sees single, printable lines.
func splitLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		// A line handed over with its terminator shouldn't add an empty one.
		l = strings.TrimSuffix(strings.TrimSuffix(l, "\n"), "\r")
		for _, part := range strings.Split(l, "\n") {
			out = append(out, cleanLine(part))
		}
	}
	return out
}

// ansi matches CSI sequences, OSC sequences (ended by BEL or ST), DCS, SOS, PM
// and APC strings with their payload (a terminal never shows it, so neither do
// we) and the two byte escapes.
var ansi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[PX^_][^\x1b]*(?:\x1b\\|$)|\x1b[@-Z\\-_]`)

// cleanLine strips ANSI escapes, every other control character except tab, the
// invisible format characters (bidi overrides, zero width and tag characters),
// private use characters and the Unicode line and paragraph separators. Pane
// output is untrusted. These can
// hide text or start a new line where the message doesn't expect one, so none of
// them should reach the feed or an agent.
func cleanLine(line string) string {
	line = ansi.ReplaceAllString(line, "")
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Co, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, line)
}
