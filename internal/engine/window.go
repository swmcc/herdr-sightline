package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The lines around an error. Each source keeps a rolling window of its last
// ContextLines lines, which become the context before a match. The matched line
// is the error itself. After it, the capture collects the rest of the error
// block (stack frames, "Caused by:" and the like) until one of these comes first:
//
//   - the block ends (see block.next for the rule)
//   - ContextLines lines have been collected
//   - CaptureWait has passed since the match
//
// A match on a line that carries on a block being captured is part of that
// error, so it's folded in rather than starting a capture of its own.
//
// The capture is filed CaptureWait after the match even when its block ended
// sooner. Filing on a fixed delay keeps the feed in match order across sources,
// and keeps the fix check's promise that anything matched before a settle period
// ends is filed before its verdict.
//
// Only this short window is ever kept. Sightline isn't a log store.

// maxPending caps the captures in progress on one source. A flood of matching
// lines would otherwise make every new line do work for every capture. Matches
// past the cap are dropped and the newest capture is flagged MaybeGap, because
// an error was missed while it was being captured.
const maxPending = 32

// capturer is everything the engine keeps for one source between lines: the
// scrubber's state, the window and the captures still collecting lines.
type capturer struct {
	scrub scrubber
	// shadow reads the same output but never abandons a string, so it knows what
	// a real terminal is still hiding after scrub has given up at the cap.
	shadow   scrubber
	diverged bool // shadow hid something on this line that scrub showed
	window   window
	pending  []*capture
	missed   bool // output was swallowed, so flag the next capture MaybeGap
}

func newCapturer(contextLines int) capturer {
	return capturer{window: window{size: contextLines}, shadow: newShadow()}
}

// read runs lines from the adapter through the scrubber and calls use for each
// clean line, in order. Each element is one line, with or without its line end.
//
// A line that a terminal would have hidden, even in part, is still passed on
// (so the watcher can't be blinded), but it's marked as a gap: captures in
// progress and the next capture are flagged MaybeGap.
func (c *capturer) read(lines []string, use func(line string)) {
	emit := func(line string) {
		if c.diverged || c.shadow.hiding() {
			c.missed = true
			for _, p := range c.pending {
				p.maybeGap = true
			}
		}
		c.diverged = false
		use(line)
	}
	for _, l := range lines {
		l = strings.TrimSuffix(strings.TrimSuffix(l, "\n"), "\r")
		for _, r := range l + "\n" {
			c.shadow.step(r, discardLine, noGap)
			c.scrub.step(r, emit, c.hidLines)
			// A line end has just emitted its line, and emit checks the shadow
			// itself, so it mustn't mark the next line.
			if r != '\n' && c.shadow.hiding() && !c.scrub.hiding() {
				c.diverged = true
			}
		}
	}
}

func discardLine(string) {}
func noGap()             {}

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
	c.shadow = newShadow()
	c.diverged = false
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
	block   block // whether the error block is still going
	due     time.Time
	arrival int // from arrivals, to break ties between equal times
}

// result is what the capture files: its context with any blank lines at the end
// dropped, because they only separate the error from whatever came next.
func (c *capture) result() found {
	f := c.found
	for len(f.after) > 0 && strings.TrimSpace(f.after[len(f.after)-1]) == "" {
		f.after = f.after[:len(f.after)-1]
	}
	return f
}

// maxStoredBytes caps each line the window and captures keep. Detection still
// sees the whole line. Only what's kept, and so what can reach a message, is cut.
const maxStoredBytes = 4 << 10

// capStored shortens a very long line and says so, so the cut is visible.
func capStored(line string) string {
	if len(line) <= maxStoredBytes {
		return line
	}
	cut := maxStoredBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return fmt.Sprintf("%s [%d more bytes cut]", line[:cut], len(line)-cut)
}

// observe takes one clean line from s.
func (e *Engine) observe(s *source, line string, now time.Time) {
	c := &s.capture
	line = c.unhide(line, e.startsError)
	kept := capStored(line)
	sh := shapeOf(line)
	// A line that carries on an error being captured is part of that error, so
	// it doesn't start one of its own, even if it matches. A crash that matches
	// on two of its lines (puma's "Unable to load application" then its
	// "(NameError)", or Python's exception line after a matched traceback) is
	// one entry.
	folded := false
	for _, p := range c.pending {
		if !p.block.open || len(p.after) >= e.opts.ContextLines {
			continue
		}
		p.after = append(p.after, kept)
		if p.block.next(sh) {
			folded = true
		}
		if len(p.after) >= e.opts.ContextLines {
			// Full, so anything after it matching starts its own capture.
			p.block.open = false
		}
	}
	if name, ok := e.startsError(line); ok && !folded {
		if len(c.pending) >= maxPending {
			c.pending[len(c.pending)-1].maybeGap = true
		} else {
			c.pending = append(c.pending, &capture{
				found: found{
					matcher:   name,
					lines:     []string{kept},
					before:    c.window.snapshot(),
					matchedAt: now,
					maybeGap:  c.missed,
				},
				block:   startBlock(sh),
				due:     now.Add(e.opts.CaptureWait),
				arrival: e.arrivals.take(),
			})
			c.missed = false
		}
	}
	c.window.push(kept)
}

// unhide stops a carriage return being used to blind the watcher. The scrubber
// keeps only the final text of a redrawn line, as a terminal shows it. If the
// text that was drawn over is what matches an error and the final text doesn't,
// the whole line is used instead, every character in the order it was printed,
// and it's flagged like any other line a terminal would partly hide.
func (c *capturer) unhide(line string, matches func(string) (string, bool)) string {
	full := c.scrub.overwritten
	if full == "" {
		return line
	}
	if _, ok := matches(line); ok {
		return line
	}
	if _, ok := matches(full); !ok {
		return line
	}
	c.missed = true
	for _, p := range c.pending {
		p.maybeGap = true
	}
	return full
}

// The end of an error block. After the matched line a capture keeps collecting
// while lines look like more of the same error, and stops at the first line that
// doesn't. That line is kept too, as the last line of context, because it's
// often the process saying what happened next ("exit status 2", "Node.js
// v20.11.0", "[nodemon] app crashed") and it shows the agent where the error
// stopped.
//
// A line carries on the block when it is:
//
//   - indented further than the matched line: stack frames in every language,
//     Python's source lines and carets, Rust's backtrace and its " --> "
//     locations. Further, not just indented, because some dev servers indent
//     their own logs (Next.js prints " GET /notices 500 in 87ms").
//   - a known continuation: "Caused by:", "Suppressed:", "... 12 more",
//     "During handling of the above exception", "The above exception was the
//     direct cause", "Traceback (most recent call last):", Go's "goroutine 1
//     [running]:", "[signal ...]" and "created by", Rust's "stack backtrace:"
//     and its "note: run with `RUST_BACKTRACE" hints, Next.js's "digest:"
//   - a JVM exception heading, like "java.lang.IllegalStateException: boom"
//     after a log line that says something failed
//   - a line of a source excerpt, like rustc's "12 |     let x = y;" or the
//     "> 14 |" that Node tools put on the line at fault
//   - nothing but closing brackets, like the "}" after a Node error's properties
//   - a Go function call, like "main.main()", after a goroutine heading
//   - the first line after a Python traceback that isn't one of its frames,
//     which is its exception
//   - the line after a Rust "panicked at src/main.rs:4:5:", which is its message
//
// A blank line might be either, so it's kept for now. A second blank line in a
// row ends the block, and blank lines at the end are dropped when it's filed.
//
// Anything else ends the block: a timestamped log line, a request log, a
// compiler's next message. Missing the odd unusual frame is better than padding
// every short error with unrelated lines.
type block struct {
	open      bool
	indent    int  // the matched line's indentation
	blanks    int  // blank lines in a row
	traceback bool // inside a Python traceback, waiting for its exception line
	goroutine bool // inside a Go goroutine dump, where frames aren't indented
	message   bool // the next line is a Rust panic's message
}

// startBlock opens the block for an error that matched on a line of shape sh.
func startBlock(sh shape) block {
	b := block{open: true, indent: sh.indent}
	b.note(sh)
	return b
}

// next records the line after the last one kept, which is kept whatever it is.
// It reports whether that line carried on the block, and closes the block if it
// ended it.
func (b *block) next(sh shape) bool {
	if sh.kind == shapeBlank {
		b.blanks++
		b.open = b.blanks < 2
		return b.open
	}
	b.blanks = 0
	message := b.message
	b.message = false
	deeper := sh.kind == shapeIndented && sh.indent > b.indent
	switch {
	case b.traceback && !deeper && sh.kind != shapeTraceback:
		// The exception at the end of a Python traceback.
		b.traceback = false
		return true
	case message, deeper, sh.kind.continues(), sh.kind == shapeGoFunc && b.goroutine:
		b.note(sh)
		return true
	}
	b.open = false
	return false
}

// note updates what the block expects next after a line of shape sh.
func (b *block) note(sh shape) {
	switch sh.kind {
	case shapeTraceback:
		b.traceback = true
	case shapeGoroutine:
		b.goroutine = true
	case shapeRustPanic:
		b.message = true
	}
}

// shape is what a line looks like, worked out once and shared by every capture
// in progress.
type shape struct {
	kind   shapeKind
	indent int // columns of leading space, with tab stops every 8
}

type shapeKind uint8

const (
	shapeOther      shapeKind = iota // could be anything, so it ends a block
	shapeBlank                       // empty or only spaces
	shapeIndented                    // starts with a space or a tab
	shapeMarker                      // a known continuation line
	shapeTraceback                   // "Traceback (most recent call last):"
	shapeGoroutine                   // "goroutine 1 [running]:"
	shapeGoFunc                      // "main.main()", a Go frame's function line
	shapeRustPanic                   // "thread 'main' panicked at src/main.rs:4:5:"
	shapeJVMHeading                  // "java.lang.IllegalStateException: boom"
	shapeSource                      // "12 |     let x = y;" or "> 14 |"
	shapeClosing                     // "}" or "})"
)

// continues reports whether a line of this kind carries on any block, wherever
// it starts.
func (k shapeKind) continues() bool {
	switch k {
	case shapeMarker, shapeTraceback, shapeGoroutine, shapeRustPanic,
		shapeJVMHeading, shapeSource, shapeClosing:
		return true
	}
	return false
}

// continuations start lines that carry on an error block in some language.
var continuations = []string{
	"Caused by:",
	"Suppressed:",
	"During handling of the above exception",
	"The above exception was the direct cause",
	"stack backtrace:",
	"note: run with `RUST_BACKTRACE",
	"note: Some details are omitted",
	"[signal ",
	"created by ",
	`digest: "`,
}

var (
	goroutineHeading = regexp.MustCompile(`^goroutine \d+ \[[^\]]*\]:$`)
	jvmHeading       = regexp.MustCompile(`^(?:[A-Za-z_$][\w$]*\.)+[A-Z][\w$]*(?:Exception|Error|Throwable)(?::|$)`)
	sourceExcerpt    = regexp.MustCompile(`^>? *\d+ +\|`)
	jvmMore          = regexp.MustCompile(`^\.\.\. \d+ (?:more|common frames omitted)$`)
)

func shapeOf(line string) shape {
	switch {
	case strings.TrimSpace(line) == "":
		return shape{kind: shapeBlank}
	case line[0] == ' ' || line[0] == '\t':
		return shape{kind: shapeIndented, indent: indentOf(line)}
	case line == "Traceback (most recent call last):":
		return shape{kind: shapeTraceback}
	case goroutineHeading.MatchString(line):
		return shape{kind: shapeGoroutine}
	case strings.Contains(line, " panicked at ") && strings.HasSuffix(line, ":"):
		return shape{kind: shapeRustPanic}
	case jvmMore.MatchString(line):
		return shape{kind: shapeMarker}
	case strings.Trim(line, "}]);, ") == "":
		return shape{kind: shapeClosing}
	case sourceExcerpt.MatchString(line):
		return shape{kind: shapeSource}
	case jvmHeading.MatchString(line):
		return shape{kind: shapeJVMHeading}
	case isGoFunc(line):
		return shape{kind: shapeGoFunc}
	}
	for _, c := range continuations {
		if strings.HasPrefix(line, c) {
			return shape{kind: shapeMarker}
		}
	}
	return shape{kind: shapeOther}
}

// indentOf counts the columns of space at the start of line, as a terminal with
// a tab stop every 8 columns lays it out.
func indentOf(line string) int {
	n := 0
	for _, r := range line {
		switch r {
		case ' ':
			n++
		case '\t':
			n = n/8*8 + 8
		default:
			return n
		}
	}
	return n
}

// isGoFunc reports whether line looks like the function line of a Go frame: a
// name with no spaces, then its arguments in brackets to the end of the line.
func isGoFunc(line string) bool {
	open := strings.IndexByte(line, '(')
	return open > 0 && strings.HasSuffix(line, ")") && !strings.ContainsAny(line[:open], " \t")
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
		e.group(f.src, f.c.result())
	}
}

// fileAll files every capture on s now, however much context it has. Used when
// a source stops being watched.
func (e *Engine) fileAll(s *source) {
	for _, c := range s.capture.pending {
		e.group(s.info, c.result())
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
// Redrawn lines. A progress bar or spinner rewrites its line with a carriage
// return, so a terminal shows only the final text. The scrubber follows that, so
// "50%\r100%" is kept as "100%", not "50%100%". Dropping the text that was drawn
// over can't smuggle anything to a model, because the model only ever gets what
// the message holds, and that's what `v` shows. What it could do is hide an error
// from the watcher, so observe checks the drawn over text too (see unhide).
//
// It only follows a line while it's sure where the cursor is. That means plain
// ASCII and Braille (spinners), colour codes, a carriage return, a backspace,
// erase in line (CSI K) and a move to a column already written (CSI G). Anything
// else, such as a tab, a character that might be wide, another cursor movement, a
// control string or any other escape, stops it following that line, and the rest
// of the line is kept as printed, with nothing drawn over. Being unsure always
// keeps more text, never less. It assumes the line doesn't wrap. herdr's reads
// are already drawn by its terminal, so in practice this only matters for a raw
// stream.

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
	// shadow means only the state is tracked: no text is kept and strings are
	// never abandoned. Its work per rune is constant, so it can't be made slow.
	shadow bool
	// csiParam and csiPlain read the current CSI's parameter, for the two that
	// redraw a line: erase in line and move to a column. csiPlain is false once
	// anything but a digit has been seen.
	csiParam int
	csiPlain bool
	screen   screen
	// overwritten is every character of the line just emitted, in the order
	// printed, when that's different from what a terminal shows. Otherwise it's
	// empty. It's only meaningful while emit runs.
	overwritten string
}

const (
	maxCSIParam   = 1 << 16  // a bigger parameter is read as this
	maxRedrawWork = 64 << 10 // the most cells erasing may blank on one line
)

// screen is the current line as a terminal shows it, once it has been redrawn.
// Until then the line is exactly what scrubber.line holds.
type screen struct {
	active    bool // the line has been redrawn, so cells is what's shown
	untracked bool // the cursor moved in a way that isn't followed
	cells     []rune
	cursor    int
	work      int // cells blanked so far, against maxRedrawWork
}

// narrow reports whether r is certain to take exactly one column in any
// terminal. Everything else might be wide, or zero width, or depend on settings.
func narrow(r rune) bool {
	return r >= 0x20 && r < ctrlDEL || r >= 0x2800 && r <= 0x28ff
}

// untrack stops following the line. The rest of it is kept as printed.
func (s *scrubber) untrack() {
	s.screen.untracked = true
}

// redraw gets the screen ready for the cursor to move, and reports false if the
// line isn't being followed.
func (s *scrubber) redraw() bool {
	if s.shadow || s.screen.untracked {
		return false
	}
	if !s.screen.active {
		s.screen.active = true
		s.screen.cells = []rune(s.line.String())
		s.screen.cursor = len(s.screen.cells)
	}
	return true
}

// moveTo puts the cursor at col, counting from 0. A column past the end of the
// line would leave blank cells before the next character, so it isn't followed.
func (s *scrubber) moveTo(col int) {
	if !s.redraw() {
		return
	}
	if col > len(s.screen.cells) {
		s.untrack()
		return
	}
	s.screen.cursor = max(col, 0)
}

func (s *scrubber) backspace() {
	if s.redraw() {
		s.screen.cursor = max(s.screen.cursor-1, 0)
	}
}

// eraseLine is CSI K: 0 erases from the cursor to the end of the line, 1 from
// the start to the cursor and 2 the whole line. The cursor doesn't move.
func (s *scrubber) eraseLine(mode int) {
	if !s.redraw() {
		return
	}
	sc := &s.screen
	if mode == 0 || mode == 2 {
		sc.cells = sc.cells[:sc.cursor]
	}
	if mode == 0 {
		return
	}
	n := min(sc.cursor+1, len(sc.cells))
	sc.work += n
	if sc.work > maxRedrawWork {
		// Erasing over and over could make each line slow. Keep the rest instead.
		s.untrack()
		return
	}
	for i := range n {
		sc.cells[i] = ' '
	}
}

// put writes a visible character where the cursor is.
func (s *scrubber) put(r rune) {
	sc := &s.screen
	if !narrow(r) {
		sc.untracked = true
	}
	switch {
	case !sc.active:
	case sc.untracked:
		sc.cells = append(sc.cells, r)
	case sc.cursor == len(sc.cells):
		sc.cells = append(sc.cells, r)
		sc.cursor++
	default:
		sc.cells[sc.cursor] = r
		sc.cursor++
	}
}

func (s *scrubber) startCSI() {
	s.state, s.csiBytes = scrubCSI, 0
	s.csiParam, s.csiPlain = 0, true
}

// csiByte reads a parameter or intermediate byte of a CSI.
func (s *scrubber) csiByte(r rune) {
	if s.csiPlain && r >= '0' && r <= '9' {
		s.csiParam = min(s.csiParam*10+int(r-'0'), maxCSIParam)
		return
	}
	s.csiPlain = false
}

// csiFinal acts on the final byte of a CSI. Colour changes nothing on the line.
// Erase in line and a move to a column are followed. Anything else may move the
// cursor, so the line stops being followed.
func (s *scrubber) csiFinal(r rune) {
	switch {
	case s.state == scrubCSIIgnore:
		s.untrack()
	case r == 'm':
	case r == 'K' && s.csiPlain && s.csiParam <= 2:
		s.eraseLine(s.csiParam)
	case r == 'G' && s.csiPlain:
		s.moveTo(max(s.csiParam, 1) - 1)
	default:
		s.untrack()
	}
}

func newShadow() scrubber { return scrubber{shadow: true} }

// hiding reports whether the scrubber is inside a control string.
func (s *scrubber) hiding() bool {
	return s.state == scrubOSC || s.state == scrubString || s.state == scrubStringEsc
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
			s.startCSI()
		case r == ']':
			s.state = scrubOSC
		case r == 'P', r == 'X', r == '^', r == '_':
			s.state = scrubString
		case r >= 0x20 && r <= 0x2f:
			s.state = scrubEscInter
			// ESC ( to ESC / pick a character set and leave the cursor alone.
			if r < '(' {
				s.untrack()
			}
		case r < 0x20 || r == ctrlDEL:
			s.control(r, emit, hid)
		default:
			// The final byte of a two byte escape, or junk. Neither is shown,
			// but some move the cursor.
			s.untrack()
			s.state = scrubText
		}

	case scrubEscInter:
		switch {
		case r >= 0x20 && r <= 0x2f:
			s.untrack()
		case r < 0x20 || r == ctrlDEL:
			s.control(r, emit, hid)
		default:
			s.state = scrubText
		}

	case scrubCSI, scrubCSIIgnore:
		s.csiBytes++
		switch {
		case r >= 0x40 && r <= 0x7e:
			s.csiFinal(r)
			s.state = scrubText
		case r < 0x20 || r == ctrlDEL:
			s.control(r, emit, hid)
		case r >= 0x20 && r <= 0x3f:
			s.csiByte(r)
			if s.csiBytes > maxCSIBytes {
				s.state = scrubCSIIgnore
			}
		default:
			// Not ASCII, so not part of any sequence. Abandon it unshown.
			s.untrack()
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
		// Cancel any sequence. Nothing is shown here, though some terminals
		// draw a character for SUB, so the line stops being followed.
		s.state = scrubText
		s.untrack()
	case r == ctrlESC && !inString:
		s.state = scrubEsc
	case r == c1CSI:
		s.startCSI()
	case r == c1OSC:
		s.state = scrubOSC
	case r == c1DCS, r == c1SOS, r == c1PM, r == c1APC:
		s.state = scrubString
	case r >= 0x80 && r <= 0x9f:
		// Any other C1 control is acted on and never shown. A terminal also ends a
		// string here, but carrying on swallowing is the safe way to differ: it
		// can only hide more, and the cap bounds that. Some, like NEL and RI,
		// move the cursor to another line.
		if !inString {
			s.state = scrubText
		}
		s.untrack()
	default:
		return false
	}
	return true
}

// control handles a C0 control or DEL inside an escape or CSI. A terminal acts on
// it and stays in the sequence. A line end ends the line. The sequence then
// carries on into the next line and eats its first characters, as a terminal
// does, which could quietly defeat a pattern anchored at the start of a line. So
// it counts as a gap. The controls that move the cursor along the line are
// followed as they are outside a sequence.
func (s *scrubber) control(r rune, emit func(string), hid func()) {
	switch r {
	case '\n':
		s.endLine(emit)
		hid()
	case '\r', '\b', '\t', '\v', '\f':
		s.cursorControl(r)
	}
}

// cursorControl follows a carriage return or backspace. A tab, vertical tab or
// form feed moves the cursor somewhere that isn't followed.
func (s *scrubber) cursorControl(r rune) {
	switch r {
	case '\r':
		s.moveTo(0)
	case '\b':
		s.backspace()
	default:
		s.untrack()
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
	// A string can draw (sixel) or do things a line can't follow, so the rest of
	// this line is kept as printed. That includes the line after a hidden line
	// end, which a terminal shows on the same row, after the text before it.
	s.untrack()
	if s.shadow || s.hiddenBytes <= maxHiddenBytes && s.hiddenLines <= maxHiddenLines {
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
	if s.shadow {
		return
	}
	switch {
	case r == '\n':
		s.endLine(emit)
	case r == '\t':
		s.visible(r)
		s.untrack()
	case r == '\r', r == '\b', r == '\v', r == '\f':
		s.cursorControl(r)
	case unicode.In(r, invisible...):
	default:
		s.visible(r)
	}
}

func (s *scrubber) visible(r rune) {
	s.line.WriteRune(r)
	s.put(r)
	s.hiddenBytes, s.hiddenLines = 0, 0
}

// endLine emits the line as a terminal shows it. If that's different from what
// was printed, everything printed is left in overwritten while emit runs.
func (s *scrubber) endLine(emit func(string)) {
	if s.shadow {
		return
	}
	shown := s.line.String()
	s.overwritten = ""
	if s.screen.active {
		if redrawn := string(s.screen.cells); redrawn != shown {
			s.overwritten, shown = shown, redrawn
		}
	}
	s.line.Reset()
	s.screen = screen{}
	emit(shown)
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
//
// It joins the lines with nothing between them, so text either side of a hidden
// string can run together ("Err" and "or" make "Error"). That's harmless for
// titles and for lines the stream has already scrubbed, but it must never be
// used on anything that reaches detection. Pane output goes through
// capturer.read instead.
func cleanLine(s string) string {
	var sc scrubber
	var b strings.Builder
	sc.write(s+"\n", func(l string) { b.WriteString(l) }, func() {})
	b.WriteString(sc.line.String())
	return b.String()
}
