package engine

import (
	"regexp"
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

// capture is an error that has matched but is still collecting the lines after it.
type capture struct {
	lines     []string
	before    []string
	after     []string
	matchedAt time.Time
	due       time.Time
	maybeGap  bool
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
		})
	}
	s.window.push(line)
}

// finishCaptures turns every capture on s whose wait is over into an entry.
func (e *Engine) finishCaptures(s *source, now time.Time) {
	kept := s.pending[:0]
	for _, c := range s.pending {
		if now.Before(c.due) {
			kept = append(kept, c)
			continue
		}
		e.group(s.info, c)
	}
	clear(s.pending[len(kept):])
	s.pending = kept
}

// splitLines breaks any embedded newlines out and cleans every line, so the rest
// of the engine only ever sees single, printable lines.
func splitLines(lines []string) []string {
	var out []string
	for _, l := range lines {
		for _, part := range strings.Split(l, "\n") {
			out = append(out, cleanLine(part))
		}
	}
	return out
}

// ansi matches CSI sequences, OSC sequences (ended by BEL or ST) and the two
// byte escapes.
var ansi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// cleanLine strips ANSI escapes and every other control character except tab.
// Pane output is untrusted, and none of it should reach the feed or an agent.
func cleanLine(line string) string {
	line = ansi.ReplaceAllString(line, "")
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, line)
}
