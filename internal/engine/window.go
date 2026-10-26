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
		c.scrub.write(l+"\n", use, c.hidLines)
	}
}

// hidLines is called when the scrubber may have hidden whole lines, inside a
// control string or by giving up on one. Either way it's a gap.
func (c *capturer) hidLines() {
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
// across two Feed calls leak its tail. The states and transitions follow the
// DEC VT500 parser that xterm and VTE are built on, including the controls that
// act from any state.
//
// One known gap, left for #8: a carriage return or cursor movement that
// overwrites text on screen isn't interpreted, so both the old and new text are
// kept. That fails towards showing more, never less, and `v` shows it.

type scrubState uint8

const (
	scrubText      scrubState = iota
	scrubEsc                  // after ESC
	scrubEscInter             // after ESC and an intermediate byte, as in ESC ( B
	scrubCSI                  // a control sequence: ESC [ or C1 CSI
	scrubCSIIgnore            // a control sequence too long to be real, swallowed to its end
	scrubOSC                  // an operating system command: ESC ] or C1 OSC
	scrubString               // DCS, SOS, PM or APC
	scrubStringEsc            // ESC inside OSC or one of the strings, maybe the start of ST
	scrubDropLine             // the rest of a line after a control string was abandoned
)

const (
	maxCSIBytes      = 64       // a longer CSI is junk, so it's swallowed to its final byte
	maxHiddenBytes   = 64 << 10 // the most output control strings may hide in a row
	maxHiddenLines   = 100      // the most line ends control strings may hide in a row
	ctrlCAN, ctrlSUB = 0x18, 0x1a
	ctrlESC, ctrlBEL = 0x1b, 0x07
	ctrlDEL          = 0x7f
	c1DCS, c1SOS     = 0x90, 0x98
	c1CSI, c1ST      = 0x9b, 0x9c
	c1OSC            = 0x9d
	c1PM, c1APC      = 0x9e, 0x9f
)

// scrubber turns a stream of terminal output into clean lines.
type scrubber struct {
	state    scrubState
	line     strings.Builder
	csiBytes int // bytes in the current CSI
	// hiddenBytes and hiddenLines count what control strings have swallowed since
	// the last visible character. Starting a new string doesn't reset them, so
	// output that keeps reopening strings still hits the cap.
	hiddenBytes int
	hiddenLines int
}

// write runs text through the scrubber. emit gets each complete, clean line.
// hid is called whenever lines may have gone missing: each time a control string
// swallows a line end, and when control strings hide more than the cap. At the
// cap the scrubber abandons the string, drops the rest of that line and carries
// on, so output that never closes a string can't blind the watcher for ever.
func (s *scrubber) write(text string, emit func(string), hid func()) {
	for _, r := range text {
		s.step(r, emit, hid)
	}
}

func (s *scrubber) step(r rune, emit func(string), hid func()) {
	if s.anywhere(r) {
		return
	}
	switch s.state {
	case scrubText:
		s.text(r, emit)

	case scrubEsc:
		switch {
		case r == '[':
			s.state, s.csiBytes = scrubCSI, 0
		case r == ']':
			s.state = scrubOSC
		case r == 'P', r == 'X', r == '^', r == '_':
			s.state = scrubString
		case r >= 0x20 && r <= 0x2f:
			s.state = scrubEscInter
		case r < 0x20 || r == ctrlDEL:
			s.control(r, emit)
		default:
			// The final byte of a two byte escape, or junk. Neither is shown.
			s.state = scrubText
		}

	case scrubEscInter:
		switch {
		case r >= 0x20 && r <= 0x2f:
		case r < 0x20 || r == ctrlDEL:
			s.control(r, emit)
		default:
			s.state = scrubText
		}

	case scrubCSI, scrubCSIIgnore:
		s.csiBytes++
		switch {
		case r >= 0x40 && r <= 0x7e:
			s.state = scrubText
		case r < 0x20 || r == ctrlDEL:
			s.control(r, emit)
		case r >= 0x20 && r <= 0x3f:
			if s.csiBytes > maxCSIBytes {
				s.state = scrubCSIIgnore
			}
		default:
			// Not ASCII, so not part of any sequence. Abandon it unshown.
			s.state = scrubText
		}

	case scrubOSC, scrubString:
		switch {
		case r == ctrlBEL && s.state == scrubOSC:
			s.state = scrubText
		case r == ctrlESC:
			s.state = scrubStringEsc
		default:
			s.swallow(r, emit, hid)
		}

	case scrubStringEsc:
		if r == '\\' {
			s.state = scrubText
			return
		}
		// ESC followed by anything else cancels the string and starts a new
		// escape, as it does in a terminal.
		s.state = scrubEsc
		s.step(r, emit, hid)

	case scrubDropLine:
		if r == '\n' {
			s.state = scrubText
			s.text(r, emit)
		}
	}
}

// anywhere handles the controls a terminal acts on whatever state it's in, and
// reports whether r was one of them. On a line being dropped after an abandoned
// string nothing counts, so the rest of that line can't be brought back.
func (s *scrubber) anywhere(r rune) bool {
	if s.state == scrubDropLine {
		return false
	}
	inString := s.state == scrubOSC || s.state == scrubString
	switch {
	case r == ctrlCAN, r == ctrlSUB, r == c1ST:
		// Cancel any sequence. Nothing is shown.
		s.state = scrubText
	case r == ctrlESC && !inString:
		s.state = scrubEsc
	case r == c1CSI:
		s.state, s.csiBytes = scrubCSI, 0
	case r == c1OSC:
		s.state = scrubOSC
	case r == c1DCS, r == c1SOS, r == c1PM, r == c1APC:
		s.state = scrubString
	case r >= 0x80 && r <= 0x9f:
		// Any other C1 control is acted on and never shown. A terminal also ends a
		// string here, but carrying on swallowing is the safe way to differ: it
		// can only hide more, and the cap bounds that.
		if !inString {
			s.state = scrubText
		}
	default:
		return false
	}
	return true
}

// control handles a C0 control or DEL inside an escape or CSI. A terminal acts on
// it and stays in the sequence. Only a line end matters here, because it ends a
// line.
func (s *scrubber) control(r rune, emit func(string)) {
	if r == '\n' {
		s.endLine(emit)
	}
}

// swallow drops r as part of a control string, until control strings have hidden
// too much in a row.
func (s *scrubber) swallow(r rune, emit func(string), hid func()) {
	s.hiddenBytes += utf8.RuneLen(r)
	if r == '\n' {
		s.hiddenLines++
		// Visible text either side of a hidden line end stays on separate lines,
		// so the two halves can't be joined into something that matches.
		if s.line.Len() > 0 {
			s.endLine(emit)
		}
		// Healthy output almost never hides a line end. When it happens, lines
		// may be missing, which is a gap, cap or no cap.
		hid()
	}
	if s.hiddenBytes <= maxHiddenBytes && s.hiddenLines <= maxHiddenLines {
		return
	}
	s.hiddenBytes, s.hiddenLines = 0, 0
	hid()
	if r == '\n' {
		s.state = scrubText
		return
	}
	s.state = scrubDropLine
}

// text handles r outside any escape sequence.
func (s *scrubber) text(r rune, emit func(string)) {
	switch {
	case r == '\n':
		s.endLine(emit)
	case r == '\t':
		s.visible(r)
	case unicode.In(r, invisible...):
	default:
		s.visible(r)
	}
}

func (s *scrubber) visible(r rune) {
	s.line.WriteRune(r)
	s.hiddenBytes, s.hiddenLines = 0, 0
}

func (s *scrubber) endLine(emit func(string)) {
	emit(s.line.String())
	s.line.Reset()
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
