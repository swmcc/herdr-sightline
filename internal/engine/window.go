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

// maxPending caps the captures in progress on one source. A flood of matching
// lines would otherwise make every new line do work for every capture. Matches
// past the cap are dropped and the newest capture is flagged MaybeGap, because
// an error was missed while it was being captured.
const maxPending = 32

// capturer is everything the engine keeps for one source between lines: the
// window and the captures still collecting lines.
type capturer struct {
	window  window
	pending []*capture
	seq     int // captures started, to keep ones that match at the same time in order
}

func newCapturer(contextLines int) capturer {
	return capturer{window: window{size: contextLines}}
}

// gap flags captures in progress and empties the window, so lines from before a
// gap aren't passed off as context for an error after it.
func (c *capturer) gap() {
	for _, p := range c.pending {
		p.maybeGap = true
	}
	c.window.reset()
}

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
	found
	due time.Time
	seq int
}

// observe takes one clean line from s.
func (e *Engine) observe(s *source, line string, now time.Time) {
	c := &s.capture
	for _, p := range c.pending {
		if len(p.after) < e.opts.ContextLines {
			p.after = append(p.after, line)
		}
	}
	if name, ok := e.startsError(line); ok {
		if len(c.pending) >= maxPending {
			c.pending[len(c.pending)-1].maybeGap = true
		} else {
			c.pending = append(c.pending, &capture{
				found: found{
					matcher:   name,
					lines:     []string{line},
					before:    c.window.snapshot(),
					matchedAt: now,
				},
				due: now.Add(e.opts.CaptureWait),
				seq: c.seq,
			})
			c.seq++
		}
	}
	c.window.push(line)
}

// finishCaptures files every capture whose wait is over. They're filed in the
// order they matched, across all sources, so the feed's order never depends on
// map iteration.
func (e *Engine) finishCaptures(now time.Time) {
	type finished struct {
		src Source
		c   *capture
	}
	var due []finished
	for _, s := range e.sources {
		kept := s.capture.pending[:0]
		for _, c := range s.capture.pending {
			if now.Before(c.due) {
				kept = append(kept, c)
				continue
			}
			due = append(due, finished{s.info, c})
		}
		clear(s.capture.pending[len(kept):])
		s.capture.pending = kept
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
		e.group(f.src, f.c.found)
	}
}

// fileAll files every capture on s now, however much context it has. Used when
// a source stops being watched.
func (e *Engine) fileAll(s *source) {
	for _, c := range s.capture.pending {
		e.group(s.info, c.found)
	}
	s.capture.pending = nil
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

// invisible is every character that shows nothing, or something other than
// itself, but that a model still reads. Pane output is untrusted, and these can
// smuggle text past a person or start a new line where the message doesn't
// expect one.
var invisible = []*unicode.RangeTable{
	unicode.Cc,                 // control characters
	unicode.Cf,                 // format: bidi, zero width, tags
	unicode.Co,                 // private use
	unicode.Zl,                 // line separator
	unicode.Zp,                 // paragraph separator
	unicode.Variation_Selector, // can encode a hidden payload
	unicode.Other_Default_Ignorable_Code_Point, // Hangul fillers and the like
}

// cleanLine strips ANSI escapes and every invisible character except tab.
func cleanLine(line string) string {
	line = ansi.ReplaceAllString(line, "")
	return strings.Map(func(r rune) rune {
		if r != '\t' && unicode.In(r, invisible...) {
			return -1
		}
		return r
	}, line)
}
