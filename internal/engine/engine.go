package engine

import (
	"fmt"
	"time"
)

// SourceID names where lines come from. The adapter uses the pane id. The engine
// treats it as an opaque key.
type SourceID string

// Source describes a watched source for display and for the message.
type Source struct {
	ID      SourceID
	Title   string // "web"
	Command string // "next dev"
}

// Options configure an engine. cmd/sightline builds them from sightline.toml.
type Options struct {
	Matchers     []Matcher     // built-in presets and custom patterns (#6, #13)
	Ignores      []Matcher     // never alert on these
	ContextLines int           // lines kept before and after a match (#8)
	CaptureWait  time.Duration // wait for the rest of a stack trace (#8)
	Settle       time.Duration // fix check (#12)
	Cooldown     time.Duration // crash loops (#7)
	ClosingLine  string        // last line of the message (#11)
}

// maxContextLines bounds ContextLines, and so the size of a message, until #11
// adds a cap on the whole message.
const maxContextLines = 200

// Engine turns lines of output into entries and messages.
//
// It's synchronous and owns no goroutines. Time is always passed in, never read
// from the clock. It has no locks either: one goroutine owns it and makes every
// call, which in Sightline is the feed's model.
type Engine struct {
	opts    Options
	sources map[SourceID]*source
	records []*record // oldest first
	byID    map[EntryID]*record
	lastID  int
	groups  groups // #7's bookkeeping, see group.go
}

// source is what the engine keeps for one watched source.
type source struct {
	info    Source
	capture capturer // #8's window and captures in progress, see window.go
}

// record is an entry plus the bookkeeping behind it that callers never see.
type record struct {
	entry Entry
	fix   fixState // #12's settle period, see fixcheck.go
}

// New makes an engine with the given options. ContextLines is clamped to
// between 0 and 200.
func New(opts Options) *Engine {
	opts.ContextLines = min(max(opts.ContextLines, 0), maxContextLines)
	return &Engine{
		opts:    opts,
		sources: make(map[SourceID]*source),
		byID:    make(map[EntryID]*record),
	}
}

// Watch starts accepting lines from src. Watching a source again updates its
// title and command and keeps its window.
func (e *Engine) Watch(src Source) {
	if s, ok := e.sources[src.ID]; ok {
		s.info = src
		return
	}
	e.sources[src.ID] = &source{info: src, capture: newCapturer(e.opts.ContextLines)}
}

// Unwatch stops accepting lines from id. Errors still being captured are filed
// straight away with whatever context they have. Entries already in the feed stay.
//
// After a reconnect, call Gap rather than Unwatch and Watch, so the source keeps
// its captures and they're flagged as possibly incomplete.
func (e *Engine) Unwatch(id SourceID) {
	s, ok := e.sources[id]
	if !ok {
		return
	}
	e.fileAll(s)
	delete(e.sources, id)
}

// Feed hands the engine new lines from a source, in the order they were printed.
// Lines from a source that isn't watched are ignored.
func (e *Engine) Feed(id SourceID, lines []string, now time.Time) {
	s, ok := e.sources[id]
	if !ok {
		return
	}
	// Anything already due, including a settle period that has ended, was
	// finished before these lines arrived.
	e.Tick(now)
	for _, line := range splitLines(lines) {
		e.observe(s, line, now)
		e.sawLine(id, line, now)
	}
	e.finishCaptures(now)
}

// Gap tells the engine output from id may have been missed, for example after a
// reconnect or a cleared screen. Errors still being captured are flagged, because
// their context may have a hole in it.
func (e *Engine) Gap(id SourceID, now time.Time) {
	s, ok := e.sources[id]
	if !ok {
		return
	}
	// Captures already due were complete before the gap.
	e.Tick(now)
	s.capture.gap()
}

// Tick moves waits and timers on to now. The adapter calls it regularly, whether
// or not there's new output.
func (e *Engine) Tick(now time.Time) {
	e.finishCaptures(now)
	e.checkFixes(now)
}

// MarkSent records that an entry was sent to agent. Unknown ids are ignored.
func (e *Engine) MarkSent(id EntryID, agent string, now time.Time) {
	if r, ok := e.byID[id]; ok {
		e.sent(r, agent, now)
	}
}

// AgentFinished starts the settle period for every entry sent to agent and still
// waiting on it.
func (e *Engine) AgentFinished(agent string, now time.Time) {
	e.agentFinished(agent, now)
}

// Dismiss hides an entry. A repeat inside the cooldown stays dismissed.
func (e *Engine) Dismiss(id EntryID) {
	if r, ok := e.byID[id]; ok {
		e.dismissed(r)
	}
}

// Entries returns a copy of every entry, newest first. Changing it doesn't
// change the engine.
func (e *Engine) Entries() []Entry {
	out := make([]Entry, 0, len(e.records))
	for i := len(e.records) - 1; i >= 0; i-- {
		out = append(out, e.records[i].entry.clone())
	}
	return out
}

// Message returns exactly the text that would be sent to an agent for id. The
// adapter must send it untouched, so what `v` shows is what the agent gets.
func (e *Engine) Message(id EntryID) (string, error) {
	r, ok := e.byID[id]
	if !ok {
		return "", fmt.Errorf("building message: no entry %q", id)
	}
	return buildMessage(r.entry, e.opts), nil
}

// add puts a new entry in the feed and returns its record.
func (e *Engine) add(en Entry) *record {
	e.lastID++
	en.ID = EntryID(fmt.Sprintf("e%d", e.lastID))
	r := &record{entry: en}
	e.records = append(e.records, r)
	e.byID[en.ID] = r
	return r
}
