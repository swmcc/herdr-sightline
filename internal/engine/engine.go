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
	MaxEntries   int           // entries kept before older ones are dropped (#41)
}

// maxContextLines bounds ContextLines, and so the memory and work each source
// costs. The message has its own cap, maxMessageBytes in message.go.
const maxContextLines = 200

// MaxEntries is 200 when it isn't set and is kept between 10 and 10,000. See
// trim in group.go for which entries go first.
const (
	defaultMaxEntries = 200
	minMaxEntries     = 10
	maxMaxEntries     = 10000
)

// Engine turns lines of output into entries and messages.
//
// It's synchronous and owns no goroutines. Time is always passed in, never read
// from the clock. It has no locks either: one goroutine owns it and makes every
// call, which in Sightline is the feed's model.
type Engine struct {
	opts     Options
	sources  map[SourceID]*source
	records  []*record // oldest first
	byID     map[EntryID]*record
	lastID   int
	groups   groups   // #7's bookkeeping, see group.go
	dropped  int      // entries dropped to stay within MaxEntries (#41)
	arrivals arrivals // order of matches across sources, see window.go
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
// between 0 and 200. MaxEntries of zero or less means 200, and anything else is
// clamped to between 10 and 10,000.
func New(opts Options) *Engine {
	return &Engine{
		opts:    clamp(opts),
		sources: make(map[SourceID]*source),
		byID:    make(map[EntryID]*record),
	}
}

// clamp keeps opts within the limits New describes. Reconfigure uses it too, so
// a reload can't get round them.
func clamp(opts Options) Options {
	opts.ContextLines = min(max(opts.ContextLines, 0), maxContextLines)
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = defaultMaxEntries
	}
	opts.MaxEntries = min(max(opts.MaxEntries, minMaxEntries), maxMaxEntries)
	return opts
}

// Reconfigure swaps in new options without losing entries, so a change to
// sightline.toml applies while the feed is open (#58). They're clamped as New
// clamps them. Each option takes effect like this:
//
//   - Matchers and Ignores apply from the next line fed. A capture already in
//     progress carries on and is filed as usual, except that a new entry it
//     makes is filed dismissed if an ignore rule now matches it (see group).
//   - ClosingLine applies to the next Message.
//   - ContextLines applies to captures that start after. A capture in progress
//     keeps the limit it started with, so it never loses a line it has. Each
//     source's window takes the new size straight away, so a smaller one drops
//     its oldest lines, which were only waiting to be context for a later error.
//   - CaptureWait applies to captures that start after. A capture in progress
//     keeps its wait, and the fix check still waits for it before a verdict
//     (see longestWait).
//   - Settle applies to settle periods that start after. One under way keeps
//     its length.
//   - Cooldown applies from now, to the next repeat of a dismissed entry.
//   - MaxEntries applies from now. If it shrank, entries are dropped straight
//     away in the usual order (see trim), and an entry being checked is still
//     never dropped.
//
// A new ignore rule should work straight away (#14), so any new entry whose
// error line an ignore rule matches is dismissed, and Reconfigure returns how
// many were. Only new entries. An entry that has been sent, is settling, is
// still failing or has a verdict is left alone, because it's the answer to
// whether a fix worked and an ignore rule shouldn't hide that. A line cut short
// to be stored (see capStored) is matched as it was kept.
func (e *Engine) Reconfigure(opts Options) (dismissed int) {
	e.opts = clamp(opts)
	for _, s := range e.sources {
		w := &s.capture.window
		w.size = e.opts.ContextLines
		if extra := len(w.lines) - w.size; extra > 0 {
			w.lines = w.lines[extra:]
		}
	}
	for _, r := range e.records {
		if r.entry.State == StateNew && len(r.entry.Lines) > 0 && e.ignored(r.entry.Lines[0]) {
			e.dismissed(r)
			dismissed++
		}
	}
	e.trim(nil)
	return dismissed
}

// longestWait is how long the longest capture in progress waits, and at least
// CaptureWait. The two only differ after Reconfigure has shortened CaptureWait
// while captures with the old wait are still collecting lines. The fix check
// waits this long, so a repeat matched before a settle period ends is still
// filed before its verdict.
func (e *Engine) longestWait() time.Duration {
	wait := e.opts.CaptureWait
	for _, s := range e.sources {
		for _, c := range s.capture.pending {
			wait = max(wait, c.due.Sub(c.matchedAt))
		}
	}
	return wait
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
	// Nothing more will be read, so nothing can prove a fix on id.
	e.uncertainRead(id, time.Time{}, false)
}

// Rename tells the engine a source now goes by another id. herdr gives a pane a
// new id when it moves to another workspace, but it's the same terminal, so the
// source carries on: its window, captures in progress and scrubber state move
// across, and so do its entries, their place in the grouping index and any fix
// check under way. An entry sent before the move can then still be judged on
// output that arrives under the new id. Entry ids don't change, and a rename on
// its own isn't a gap. If output may have been missed while the source moved,
// call Gap as well.
//
// It works whether or not from is watched now, so entries left behind by a
// source that was unwatched (say a read found the pane gone just before the
// move was reported) still follow it.
//
// If to is already known, because it's watched or an entry has it, nothing is
// moved. herdr doesn't reuse pane ids, so that shouldn't happen, and merging two
// sources' state could tie one pane's errors to another's output. Instead from
// is unwatched, as Unwatch does, so its settling entries end unverified rather
// than wait on output that will never come. Rename reports whether it moved
// anything, which is false then. Renaming a source to its own id does nothing.
func (e *Engine) Rename(from, to SourceID) bool {
	if from == to {
		return false
	}
	if e.known(to) {
		e.Unwatch(from)
		return false
	}
	if s, ok := e.sources[from]; ok {
		delete(e.sources, from)
		s.info.ID = to
		e.sources[to] = s
	}
	for _, r := range e.records {
		if r.entry.Source.ID == from {
			r.entry.Source.ID = to
		}
	}
	// Collected first, so the loop never meets a key it has just added.
	var moved []groupKey
	for k := range e.groups.newest {
		if k.source == from {
			moved = append(moved, k)
		}
	}
	for _, k := range moved {
		e.groups.newest[groupKey{to, k.fingerprint}] = e.groups.newest[k]
		delete(e.groups.newest, k)
	}
	return true
}

// known reports whether id is watched or named by any entry.
func (e *Engine) known(id SourceID) bool {
	if _, ok := e.sources[id]; ok {
		return true
	}
	for _, r := range e.records {
		if r.entry.Source.ID == id {
			return true
		}
	}
	return false
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
	s.capture.read(lines, func(line string) {
		e.sawLine(id, e.observe(s, line, now), now)
	})
	if s.capture.tookHidden() {
		// Swallowed output could have hidden the error coming back.
		e.uncertainRead(id, now, false)
	}
	e.finishCaptures(now)
}

// Gap tells the engine output from id may have been missed, for example after a
// reconnect or a cleared screen. Errors still being captured are flagged, because
// their context may have a hole in it. An entry settling on id can't be called
// fixed (see fixcheck.go).
//
// Call it before feeding the lines from the same read, with the same now. Those
// lines may repeat earlier ones, so an error in them isn't taken as coming back.
func (e *Engine) Gap(id SourceID, now time.Time) {
	s, ok := e.sources[id]
	if !ok {
		return
	}
	// Captures already due were complete before the gap.
	e.Tick(now)
	s.capture.lost()
	e.uncertainRead(id, now, true)
}

// Unsure tells the engine a read from id couldn't say for certain what was new,
// so output may have gone unseen. The adapter calls it for an Ambiguous read,
// before feeding that read's lines. An entry settling on id can't be called
// fixed (see fixcheck.go).
func (e *Engine) Unsure(id SourceID, now time.Time) {
	if _, ok := e.sources[id]; !ok {
		return
	}
	e.Tick(now)
	e.uncertainRead(id, now, false)
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

// AgentGone tells the engine the agent known as agent has gone: its pane
// closed or exited, the agent left it or another agent took it over. Entries
// still waiting on it become unverified, with a Note saying why.
func (e *Engine) AgentGone(agent string, now time.Time) {
	e.agentGone(agent, now)
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

// Dropped is how many entries have been dropped since the engine started, to
// keep within MaxEntries. The feed shows it, so a flood of errors can't quietly
// push older ones out of sight.
func (e *Engine) Dropped() int {
	return e.dropped
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
	e.trim(r)
	return r
}
