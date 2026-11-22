package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Queue mode (#19) sends errors without a keypress at the moment they go, so
// it's held to the rules decided in #21. The engine owns the parts that don't
// need herdr: which entries are queued, which of them may go on their own,
// how they're put in one message and how many automatic sends are allowed.
// The adapter decides when an agent is idle and which agent it is, and must
// only send what AutoSend hands back.
//
//   - Queue mode is per source and off until the adapter turns it on.
//   - Only new entries are queued. One whose output may have a gap (MaybeGap)
//     or that has an "@" the message would rewrite is held: it stays in the
//     queue but only a keypress sends it.
//   - At most MaxAutoBatch entries, all from one source, go in one message.
//   - Every automatic send is counted when AutoSend builds it, whether or not
//     it arrives. An agent gets at most one per settle period (and never two
//     within minAutoGap) and at most maxAutoPerAgent in autoWindow. All agents
//     together get at most maxAutoTotal in autoWindow.
//   - An agent still on an earlier send (an entry sent to it hasn't had its
//     verdict) gets nothing more until it has.
//   - If an entry sent automatically comes back still failing, the source is
//     halted: it falls back to asking until a keypress sends something from it
//     or queue mode is switched off and on again. The adapter halts it too when
//     it can't tell whether a send arrived.
//
// These are constants, not options, so a config file can't loosen them.
const (
	MaxAutoBatch    = 5
	minAutoGap      = 30 * time.Second
	autoWindow      = 10 * time.Minute
	maxAutoPerAgent = 3
	maxAutoTotal    = 6
	maxAutoLog      = 50
)

// Hold says why a queued entry waits for a keypress instead of going on its
// own. It's empty when the entry may go.
type Hold string

// The reasons an entry is held.
const (
	HoldGap     Hold = "some output around it may have been missed"
	HoldMention Hold = "it has an at sign that would be rewritten"
)

// AutoSent is one automatic send, for the feed to show what went and when.
type AutoSent struct {
	Entries []EntryID
	Source  SourceID
	Agent   string
	At      time.Time
	// Text is exactly what was sent.
	Text string
	// Unsure means the send may or may not have arrived.
	Unsure bool
}

// queueState is the engine's queue bookkeeping.
type queueState struct {
	on     map[SourceID]bool
	halted map[SourceID]bool
	paused bool
	order  []*record  // queued entries, first to go first
	tries  []autoTry  // automatic sends within autoWindow, oldest first
	log    []AutoSent // newest last, at most maxAutoLog
	// aliases are the earlier names of an agent that moved (see FollowAgent).
	aliases map[string][]string
	// last is each agent's latest try, however old.
	last map[string]time.Time
}

// autoTry is one automatic send counted against the caps.
type autoTry struct {
	agent string
	at    time.Time
}

// SetQueueMode turns queue mode on or off for src. Turning it on queues the
// source's new entries, oldest first, and so does every new entry after.
// Turning it off takes them all out of the queue. Either way a halt on src is
// lifted, because switching it is a keypress.
func (e *Engine) SetQueueMode(src SourceID, on bool) {
	q := &e.queue
	delete(q.halted, src)
	if !on {
		delete(q.on, src)
		q.order = slices.DeleteFunc(q.order, func(r *record) bool { return r.entry.Source.ID == src })
		return
	}
	if q.on == nil {
		q.on = map[SourceID]bool{}
	}
	q.on[src] = true
	for _, r := range e.records {
		if r.entry.Source.ID == src {
			e.enqueue(r)
		}
	}
}

// QueueMode reports whether queue mode is on for src.
func (e *Engine) QueueMode(src SourceID) bool { return e.queue.on[src] }

// Halted reports whether queue mode is on for src but has stopped sending on
// its own until a keypress (see the rules above).
func (e *Engine) Halted(src SourceID) bool { return e.queue.halted[src] }

// Halt stops automatic sends from src until a keypress sends something from it
// or queue mode is switched. The adapter calls it when it can't tell whether a
// send arrived. It does nothing if queue mode is off for src.
func (e *Engine) Halt(src SourceID) {
	if !e.queue.on[src] {
		return
	}
	if e.queue.halted == nil {
		e.queue.halted = map[SourceID]bool{}
	}
	e.queue.halted[src] = true
}

// SetQueuePaused pauses or resumes every automatic send. Entries stay queued.
func (e *Engine) SetQueuePaused(paused bool) { e.queue.paused = paused }

// QueuePaused reports whether automatic sends are paused.
func (e *Engine) QueuePaused() bool { return e.queue.paused }

// Unqueue takes an entry out of the queue. It stays in the feed as a new entry
// and only a keypress sends it. It isn't queued again unless queue mode is
// switched off and on for its source.
func (e *Engine) Unqueue(id EntryID) {
	e.queue.order = slices.DeleteFunc(e.queue.order, func(r *record) bool { return r.entry.ID == id })
}

// MoveQueued moves a queued entry one place earlier or later in the queue. It
// reports whether it moved.
func (e *Engine) MoveQueued(id EntryID, earlier bool) bool {
	e.pruneQueue()
	o := e.queue.order
	i := slices.IndexFunc(o, func(r *record) bool { return r.entry.ID == id })
	j := i + 1
	if earlier {
		j = i - 1
	}
	if i < 0 || j < 0 || j >= len(o) {
		return false
	}
	o[i], o[j] = o[j], o[i]
	return true
}

// Queue lists the queued entries, first to go first.
func (e *Engine) Queue() []EntryID {
	e.pruneQueue()
	out := make([]EntryID, len(e.queue.order))
	for i, r := range e.queue.order {
		out[i] = r.entry.ID
	}
	return out
}

// QueueCandidates lists the queued entries from src that may go on their own
// now, first to go first. It's empty if queue mode is off, halted or paused.
// It says nothing about the agent: AutoSend checks that.
func (e *Engine) QueueCandidates(src SourceID) []EntryID {
	e.pruneQueue()
	q := &e.queue
	if !q.on[src] || q.halted[src] || q.paused {
		return nil
	}
	var out []EntryID
	for _, r := range q.order {
		if r.entry.Source.ID == src && holdOf(r.entry) == "" {
			out = append(out, r.entry.ID)
		}
	}
	return out
}

// AutoWait says why nothing can go to agent on its own at now, or "" if the
// caps would allow a send. The feed shows it on queued entries.
func (e *Engine) AutoWait(agent string, now time.Time) string {
	q := &e.queue
	q.forget(now)
	if q.paused {
		return "queue mode is paused"
	}
	if e.busy(agent) {
		return "waiting for the agent to finish what Sightline sent it"
	}
	// The latest try is kept apart from the window, so a settle period
	// longer than the window still spaces the sends out.
	last := q.last[agent]
	mine := 0
	for _, t := range q.tries {
		if t.agent == agent {
			mine++
		}
	}
	gap := max(e.opts.Settle, minAutoGap)
	switch {
	case mine >= maxAutoPerAgent:
		return fmt.Sprintf("it has had %d automatic sends in %d minutes", maxAutoPerAgent, int(autoWindow.Minutes()))
	case len(q.tries) >= maxAutoTotal:
		return fmt.Sprintf("there have been %d automatic sends in %d minutes", maxAutoTotal, int(autoWindow.Minutes()))
	case !last.IsZero() && now.Sub(last) < gap:
		return "it had an automatic send less than a settle period ago"
	}
	return ""
}

// busy reports whether agent still has an entry from an earlier send waiting
// for its verdict.
func (e *Engine) busy(agent string) bool {
	names := append([]string{agent}, e.queue.aliases[agent]...)
	for _, r := range e.records {
		if r.entry.State == StateSent && slices.Contains(names, r.entry.Agent) {
			return true
		}
	}
	return false
}

// Errors AutoSend gives when it refuses.
var (
	ErrNotQueued = errors.New("an entry isn't queued, or may not go on its own")
	ErrAutoCap   = errors.New("the caps on automatic sends don't allow it")
)

// AutoSend builds the one message queue mode may send to agent now for ids,
// and counts it against the caps. The adapter must send exactly the text it
// returns, once, and then call AutoDelivered or AutoUnsure. Nothing is counted
// if it refuses.
//
// It refuses unless there are 1 to MaxAutoBatch ids, every one is queued,
// from the same source and not held, queue mode is on for that source and not
// halted or paused, and the caps allow it (see AutoWait).
func (e *Engine) AutoSend(ids []EntryID, agent string, now time.Time) (string, error) {
	e.pruneQueue()
	if len(ids) == 0 || len(ids) > MaxAutoBatch || agent == "" {
		return "", ErrNotQueued
	}
	var batch []Entry
	for _, id := range ids {
		r, ok := e.byID[id]
		if !ok || !slices.Contains(e.queue.order, r) || holdOf(r.entry) != "" || slices.ContainsFunc(batch, func(en Entry) bool { return en.ID == id }) {
			return "", ErrNotQueued
		}
		if len(batch) > 0 && r.entry.Source.ID != batch[0].Source.ID {
			return "", ErrNotQueued
		}
		batch = append(batch, r.entry)
	}
	src := batch[0].Source.ID
	if !e.queue.on[src] || e.queue.halted[src] {
		return "", ErrNotQueued
	}
	if why := e.AutoWait(agent, now); why != "" {
		return "", fmt.Errorf("%w: %s", ErrAutoCap, why)
	}
	text := buildBatch(batch, e.opts)
	// holdOf already keeps these out. This is the message itself saying so,
	// in case the two ever disagree.
	if strings.Contains(text, mentionNote) {
		return "", ErrNotQueued
	}
	e.queue.tries = append(e.queue.tries, autoTry{agent: agent, at: now})
	if e.queue.last == nil {
		e.queue.last = map[string]time.Time{}
	}
	e.queue.last[agent] = now
	return text, nil
}

// AutoStillWanted reports whether an automatic send of ids that AutoSend
// built may still go: every entry is still queued, new and not held, and its
// source is still in queue mode, not halted or paused. The adapter asks just
// before sending, because the person may have turned queue mode off, or a
// verdict halted the pane, while it checked the agent. It counts nothing.
func (e *Engine) AutoStillWanted(ids []EntryID) bool {
	e.pruneQueue()
	q := &e.queue
	if q.paused || len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		r, ok := e.byID[id]
		if !ok || !slices.Contains(q.order, r) || holdOf(r.entry) != "" {
			return false
		}
		if src := r.entry.Source.ID; !q.on[src] || q.halted[src] {
			return false
		}
	}
	return true
}

// FollowAgent says the agent the engine knew as from is now known as to, as
// when its pane moves. The caps and the check for an earlier send follow it,
// so a move can't give an agent a fresh set of caps.
func (e *Engine) FollowAgent(from, to string) {
	q := &e.queue
	if from == to || from == "" || to == "" {
		return
	}
	for i := range q.tries {
		if q.tries[i].agent == from {
			q.tries[i].agent = to
		}
	}
	if t, ok := q.last[from]; ok {
		if t.After(q.last[to]) {
			q.last[to] = t
		}
		delete(q.last, from)
	}
	if q.aliases == nil {
		q.aliases = map[string][]string{}
	}
	q.aliases[to] = append(append(q.aliases[to], from), q.aliases[from]...)
	delete(q.aliases, from)
}

// AutoDelivered records that an automatic send of ids reached agent: they're
// sent, as MarkSent does, and marked as sent automatically.
func (e *Engine) AutoDelivered(ids []EntryID, agent, text string, now time.Time) {
	e.logAuto(ids, agent, text, now, false)
	s := e.queue.log[len(e.queue.log)-1]
	for _, id := range ids {
		r, ok := e.byID[id]
		if !ok {
			continue
		}
		e.sent(r, agent, now)
		r.entry.Auto = true
		r.entry.SentAt = now
		r.auto = &s
	}
	e.pruneQueue()
}

// AutoUnsure records that an automatic send of ids may or may not have
// reached agent. They stay as they are, and their source is halted.
func (e *Engine) AutoUnsure(ids []EntryID, agent, text string, now time.Time) {
	e.logAuto(ids, agent, text, now, true)
	for _, id := range ids {
		if r, ok := e.byID[id]; ok {
			e.Halt(r.entry.Source.ID)
			e.Unqueue(id)
		}
	}
}

func (e *Engine) logAuto(ids []EntryID, agent, text string, now time.Time, unsure bool) {
	q := &e.queue
	var src SourceID
	if len(ids) > 0 {
		if r, ok := e.byID[ids[0]]; ok {
			src = r.entry.Source.ID
		}
	}
	q.log = append(q.log, AutoSent{Entries: slices.Clone(ids), Source: src, Agent: agent, At: now, Text: text, Unsure: unsure})
	if extra := len(q.log) - maxAutoLog; extra > 0 {
		q.log = slices.Delete(q.log, 0, extra)
	}
}

// AutoLog is every automatic send kept, newest first, up to the last 50.
func (e *Engine) AutoLog() []AutoSent {
	out := make([]AutoSent, 0, len(e.queue.log))
	for _, s := range slices.Backward(e.queue.log) {
		s.Entries = slices.Clone(s.Entries)
		out = append(out, s)
	}
	return out
}

// LastAutoSend is the latest automatic send that included id, so the feed can
// show exactly what went. One that arrived is kept with the entry for as long
// as the entry is. One that may not have is only in the log.
func (e *Engine) LastAutoSend(id EntryID) (AutoSent, bool) {
	if r, ok := e.byID[id]; ok && r.auto != nil {
		s := *r.auto
		s.Entries = slices.Clone(s.Entries)
		return s, true
	}
	for _, s := range slices.Backward(e.queue.log) {
		if slices.Contains(s.Entries, id) {
			s.Entries = slices.Clone(s.Entries)
			return s, true
		}
	}
	return AutoSent{}, false
}

// enqueue puts r at the back of the queue if its source is in queue mode and
// it's new.
func (e *Engine) enqueue(r *record) {
	if r.entry.State != StateNew || !e.queue.on[r.entry.Source.ID] || slices.Contains(e.queue.order, r) {
		return
	}
	e.queue.order = append(e.queue.order, r)
}

// pruneQueue takes out entries that are no longer new or have been dropped.
func (e *Engine) pruneQueue() {
	e.queue.order = slices.DeleteFunc(e.queue.order, func(r *record) bool {
		return r.entry.State != StateNew || e.byID[r.entry.ID] != r
	})
}

// forget drops tries older than autoWindow.
func (q *queueState) forget(now time.Time) {
	q.tries = slices.DeleteFunc(q.tries, func(t autoTry) bool { return now.Sub(t.at) >= autoWindow })
}

// renameQueue follows a source to its new id.
func (e *Engine) renameQueue(from, to SourceID) {
	q := &e.queue
	for _, m := range []map[SourceID]bool{q.on, q.halted} {
		if m[from] {
			delete(m, from)
			m[to] = true
		}
	}
}

// unwatchQueue turns queue mode off for a source that's no longer watched.
// Nothing can check a fix there, so nothing should go on its own.
func (e *Engine) unwatchQueue(src SourceID) {
	e.SetQueueMode(src, false)
}

// stillFailingAfterAuto halts r's source if r went automatically.
func (e *Engine) stillFailingAfterAuto(r *record) {
	if r.entry.Auto {
		e.Halt(r.entry.Source.ID)
	}
}

// resumeOnKeypress lifts a halt on r's source, because a keypress sent it.
func (e *Engine) resumeOnKeypress(r *record) {
	delete(e.queue.halted, r.entry.Source.ID)
}

// holdOf says why en must wait for a keypress, if it must. It errs towards
// holding: any "@" in any line counts, even one the message would cut. So does
// a fullwidth one the pane printed itself, because a line holding Sightline's
// own note about "@" would otherwise make AutoSend refuse the batch on every
// try, and the pane could jam its own queue.
func holdOf(en Entry) Hold {
	if en.MaybeGap {
		return HoldGap
	}
	for _, group := range [][]string{en.Before, en.Lines, en.After} {
		for _, l := range group {
			if strings.Contains(l, "@") || strings.Contains(l, fullwidthAt) {
				return HoldMention
			}
		}
	}
	return ""
}
