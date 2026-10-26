package engine

import (
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
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
// scrubber's state, the window and the captures still collecting lines.
type capturer struct {
	scrub   scrubber
	window  window
	pending []*capture
	missed  bool // output was swallowed, so flag the next capture MaybeGap
}

func newCapturer(contextLines int) capturer {
	return capturer{window: window{size: contextLines}}
}

// read runs lines from the adapter through the scrubber and calls use for each
// clean line, in order. Each element is one line, with or without its line end.
func (c *capturer) read(lines []string, use func(line string)) {
	for _, l := range lines {
		l = strings.TrimSuffix(strings.TrimSuffix(l, "\n"), "\r")
		c.scrub.write(l+"\n", use, c.swallowedTooMuch)
	}
}

// swallowedTooMuch is called when the scrubber gives up on a control string that
// never ended. Whatever it swallowed is lost, which is a gap.
func (c *capturer) swallowedTooMuch() {
	c.gap()
	c.missed = true
}

// gap flags captures in progress and empties the window, so lines from before a
// gap aren't passed off as context for an error after it.
func (c *capturer) gap() {
	for _, p := range c.pending {
		p.maybeGap = true
	}
	c.window.reset()
}

// lost is a gap the adapter told us about. The stream may have restarted, so any
// escape sequence in progress is forgotten too.
func (c *capturer) lost() {
	c.gap()
	c.scrub = scrubber{}
}

// arrivals numbers matches across every source in the order the engine saw them,
// so captures that matched at the same time are filed in that order.
type arrivals struct {
	next int
}

func (a *arrivals) take() int {
	n := a.next
	a.next++
	return n
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
	due     time.Time
	arrival int // from arrivals, to break ties between equal times
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
					maybeGap:  c.missed,
				},
				due:     now.Add(e.opts.CaptureWait),
				arrival: e.arrivals.take(),
			})
			c.missed = false
		}
	}
	c.window.push(line)
}

// finishCaptures files every capture whose wait is over. They're filed in the
// order they matched, across all sources, with ties broken by the order the
// engine saw them, so the feed's order never depends on map iteration.
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
		return a.c.arrival < b.c.arrival
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

// Scrubbing. Pane output is untrusted, so escape sequences are removed the way a
// terminal reads them: as a stream, with state kept across lines and across Feed
// calls. Splitting into lines first would let a control string that spans lines
// (which a terminal never shows) leak its middle lines, or let a sequence split
// across two Feed calls leak its tail.

type scrubState uint8

const (
	scrubText      scrubState = iota
	scrubEsc                  // after ESC
	scrubEscInter             // after ESC and an intermediate byte, as in ESC ( B
	scrubCSI                  // a control sequence: ESC [ or C1 CSI
	scrubOSC                  // an operating system command: ESC ] or C1 OSC
	scrubString               // DCS, SOS, PM or APC
	scrubStringEsc            // ESC inside OSC or one of the strings, maybe the start of ST
)

const (
	maxCSIBytes     = 64       // a longer CSI is junk, so stop treating it as one
	maxSwallowBytes = 64 << 10 // the most a control string may swallow
	maxSwallowLines = 100      // the most line ends a control string may swallow
)

// scrubber turns a stream of terminal output into clean lines.
type scrubber struct {
	state    scrubState
	inOSC    bool // in scrubStringEsc, whether the string was an OSC
	line     strings.Builder
	seqBytes int // bytes in the current CSI or control string
	seqLines int // line ends swallowed by the current control string
}

// write runs text through the scrubber. emit gets each complete, clean line.
// overflow is called if a control string runs past its cap, at which point the
// scrubber gives up on it and carries on as if it had ended, so one unterminated
// sequence can't blind the watcher for ever.
func (s *scrubber) write(text string, emit func(string), overflow func()) {
	for _, r := range text {
		s.step(r, emit, overflow)
	}
}

func (s *scrubber) enter(st scrubState) {
	s.state = st
	s.seqBytes = 0
	s.seqLines = 0
}

func (s *scrubber) step(r rune, emit func(string), overflow func()) {
	switch s.state {
	case scrubText:
		s.text(r, emit)

	case scrubEsc:
		switch {
		case r == '[':
			s.enter(scrubCSI)
		case r == ']':
			s.enter(scrubOSC)
		case r == 'P', r == 'X', r == '^', r == '_':
			s.enter(scrubString)
		case r >= 0x20 && r <= 0x2f:
			s.state = scrubEscInter
		case r == 0x1b:
			// ESC ESC: the first is abandoned, the second starts again.
		case r == '\n':
			// A terminal acts on a line end inside an escape and stays in it, so
			// one split across Feed calls still ends where it should.
			s.text(r, emit)
		default:
			// The final byte of a two byte escape, or junk. Neither is shown.
			s.state = scrubText
		}

	case scrubEscInter:
		switch {
		case r >= 0x20 && r <= 0x2f:
		case r == '\n':
			s.text(r, emit)
		default:
			s.state = scrubText
		}

	case scrubCSI:
		s.seqBytes++
		switch {
		case r >= 0x40 && r <= 0x7e:
			s.state = scrubText
		case r == '\n':
			// A terminal acts on a line end inside a CSI and stays in the sequence.
			s.text(r, emit)
		case r == 0x1b:
			s.state = scrubEsc
		case r >= 0x20 && r <= 0x3f:
			if s.seqBytes > maxCSIBytes {
				s.state = scrubText
			}
		case r < 0x20:
			// Other C0 controls are acted on, never shown.
		default:
			s.state = scrubText
		}

	case scrubOSC, scrubString:
		switch {
		case r == 0x9c: // C1 ST
			s.state = scrubText
		case r == 0x07 && s.state == scrubOSC:
			s.state = scrubText
		case r == 0x1b:
			s.inOSC = s.state == scrubOSC
			s.state = scrubStringEsc
		default:
			s.swallow(r, emit, overflow)
		}

	case scrubStringEsc:
		if r == '\\' {
			s.state = scrubText
			return
		}
		// ESC followed by anything else cancels the string and starts a new
		// escape, as it does in a terminal.
		s.state = scrubEsc
		s.step(r, emit, overflow)
	}
}

// swallow drops r as part of a control string, until the string has swallowed
// too much.
func (s *scrubber) swallow(r rune, emit func(string), overflow func()) {
	s.seqBytes += utf8.RuneLen(r)
	if r == '\n' {
		s.seqLines++
	}
	if s.seqBytes <= maxSwallowBytes && s.seqLines <= maxSwallowLines {
		return
	}
	s.state = scrubText
	overflow()
	s.text(r, emit)
}

// text handles r outside any escape sequence.
func (s *scrubber) text(r rune, emit func(string)) {
	switch {
	case r == '\n':
		emit(s.line.String())
		s.line.Reset()
	case r == 0x1b:
		s.state = scrubEsc
	case r == 0x9b:
		s.enter(scrubCSI)
	case r == 0x9d:
		s.enter(scrubOSC)
	case r == 0x90, r == 0x98, r == 0x9e, r == 0x9f: // C1 DCS, SOS, PM and APC
		s.enter(scrubString)
	case r == '\t':
		s.line.WriteRune(r)
	case unicode.In(r, invisible...):
	default:
		s.line.WriteRune(r)
	}
}

// invisible is every character that shows nothing, or something other than
// itself, but that a model still reads. These can smuggle text past a person or
// start a new line where the message doesn't expect one.
var invisible = []*unicode.RangeTable{
	unicode.Cc,                 // control characters
	unicode.Cf,                 // format: bidi, zero width, tags
	unicode.Co,                 // private use
	unicode.Zl,                 // line separator
	unicode.Zp,                 // paragraph separator
	unicode.Variation_Selector, // can encode a hidden payload
	unicode.Other_Default_Ignorable_Code_Point, // Hangul fillers and the like
}

// cleanLine scrubs a single string on its own, such as a pane title, joining any
// lines in it. An escape sequence that never ends swallows the rest, but text
// before it on the same line is kept, as a terminal would show it.
func cleanLine(s string) string {
	var sc scrubber
	var b strings.Builder
	sc.write(s+"\n", func(l string) { b.WriteString(l) }, func() {})
	b.WriteString(sc.line.String())
	return b.String()
}
