package feed

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/herdr"
	"github.com/swmcc/herdr-sightline/internal/watchlist"
)

// Who owns what
//
// The engine and the agent tracker's view of the world belong to the model, and
// only the Bubble Tea update loop touches them. Everything that waits on herdr,
// the clock or a file runs in a loop below. Each loop owns its own state (a pane
// loop owns its PaneWatcher) and reports back by sending a message to the
// model. None of them shares memory with the model, except the queue of pane
// moves the model hands the watch list loop to write (see noteMove).
//
// Every loop runs under one root context and is counted in one WaitGroup.
// Quitting the feed cancels the root, and Run waits for the group, so nothing
// outlives the feed. A pane or agent loop also gets its own child context, so the
// model can stop one when its pane is unwatched or closes.
//
// The only other goroutines are the Bubble Tea commands that check a target and
// send a prompt. They run with the root context too, so they end with it.
//
// A panic in any of these goroutines, or in the update or view, is caught by
// faults. It keeps the first panic as an error and cancels the root, so every
// loop stops and Bubble Tea stops and puts the terminal back. Run returns the
// error once the WaitGroup is done.

// Timings is how often the loops wake up. Zero values mean the defaults.
type Timings struct {
	// Poll is how often each watched pane is read. Default 500ms.
	Poll time.Duration
	// EarlyRead is the shortest gap between two reads when a scroll hint asks
	// for one sooner. herdr batches scroll events about 100ms apart, and the
	// watcher's credit for late hints only lasts one read, so reading faster
	// than this would spend it too soon. Default 100ms.
	EarlyRead time.Duration
	// Sync is how often the agent tracker gets a fresh snapshot. Default 3s.
	Sync time.Duration
	// Tick is how often the engine's timers are moved on. Default 500ms.
	Tick time.Duration
	// List is how often the watch list file is checked. Default 1s.
	List time.Duration
	// Config is how often sightline.toml is checked. Default 1s.
	Config time.Duration
}

func (t Timings) withDefaults() Timings {
	or := func(d, def time.Duration) time.Duration {
		if d <= 0 {
			return def
		}
		return d
	}
	return Timings{
		Poll:      or(t.Poll, 500*time.Millisecond),
		EarlyRead: or(t.EarlyRead, 100*time.Millisecond),
		Sync:      or(t.Sync, 3*time.Second),
		Tick:      or(t.Tick, 500*time.Millisecond),
		List:      or(t.List, time.Second),
		Config:    or(t.Config, time.Second),
	}
}

// loops starts and stops the feed's goroutines. Its methods are only called
// from the model's update loop and from Run, so its maps need no lock. The
// WaitGroup is the one thing shared with the goroutines themselves.
type loops struct {
	ctx    context.Context
	faults *faults
	client *herdr.Client
	send   func(tea.Msg)
	now    func() time.Time
	every  Timings
	wg     sync.WaitGroup

	panes  map[string]paneLoop           // watched pane id to its loop
	gen    uint64                        // numbers each pane loop started
	agents map[string]context.CancelFunc // agent pane id to its status loop
	resync chan struct{}                 // asks the sync loop for a snapshot now

	// moves are pane moves waiting for the watch list loop to write them to
	// the file, oldest first. The model adds to them and the loop takes them,
	// so they're the one thing here behind a lock. moved wakes the loop.
	movesMu sync.Mutex
	moves   []paneMove
	moved   chan struct{}
}

// paneMove is a pane that moved from one id to another.
type paneMove struct{ from, to string }

func newLoops(ctx context.Context, f *faults, client *herdr.Client, send func(tea.Msg), now func() time.Time, every Timings) *loops {
	return &loops{
		ctx:    ctx,
		faults: f,
		client: client,
		send:   send,
		now:    now,
		every:  every.withDefaults(),
		panes:  map[string]paneLoop{},
		agents: map[string]context.CancelFunc{},
		resync: make(chan struct{}, 1),
		moved:  make(chan struct{}, 1),
	}
}

// goLoop runs fn in a goroutine counted in the WaitGroup. name says which loop
// it is if it panics.
func (l *loops) goLoop(name string, fn func()) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer l.faults.catch(name)
		panicPoint(l.ctx, name)
		fn()
	}()
}

// wait blocks until every loop has stopped. Call it after cancelling the root.
func (l *loops) wait() { l.wg.Wait() }

// deliver hands msg to the model unless the feed is shutting down.
func (l *loops) deliver(ctx context.Context, msg tea.Msg) {
	if ctx.Err() != nil {
		return
	}
	l.send(msg)
}

// startShared starts the loops that live as long as the feed: the engine tick,
// the agent sync, the watch list, the config file and herdr's global events.
func (l *loops) startShared(stateDir string, extra []string, configUpdates <-chan config.Update) {
	l.goLoop("the tick loop", l.runTick)
	l.goLoop("the sync loop", l.runSync)
	l.goLoop("the watch list loop", func() { l.runWatchList(stateDir, extra) })
	if configUpdates != nil {
		l.goLoop("the config loop", func() {
			// If forwarding panics, keep reading until Watch closes the
			// channel, so waiting for this loop still waits for Watch's
			// goroutine. The catch has to run first: it's what cancels the
			// root, and Watch only closes the channel once that happens.
			defer func() {
				for range configUpdates {
				}
			}()
			defer l.faults.catch("forwarding config updates")
			panicPoint(l.ctx, "forwarding config updates")
			l.runConfig(configUpdates)
		})
	}
	l.goLoop("the pane events loop", l.runGlobalEvents)
}

// requestSync asks the sync loop for a snapshot straight away. It never blocks:
// if one is already asked for, that's enough.
func (l *loops) requestSync() {
	select {
	case l.resync <- struct{}{}:
	default:
	}
}

// paneLoop is one running pane loop. gen tells its messages apart from those
// of an earlier loop for the same pane, which may still arrive after it was
// stopped, so a pane unwatched and watched again can't get the old loop's news.
type paneLoop struct {
	cancel context.CancelFunc
	gen    uint64
}

// watchPane starts the loop for a watched pane, unless it's already running.
func (l *loops) watchPane(pane string) {
	if _, ok := l.panes[pane]; ok {
		return
	}
	ctx, cancel := context.WithCancel(l.ctx)
	l.gen++
	gen := l.gen
	l.panes[pane] = paneLoop{cancel: cancel, gen: gen}
	l.goLoop("the loop for pane "+pane, func() { l.runPane(ctx, pane, gen) })
}

// unwatchPane stops a pane's loop. It doesn't wait: the loop sees its context
// end, and anything it sends after fails current.
func (l *loops) unwatchPane(pane string) {
	if p, ok := l.panes[pane]; ok {
		p.cancel()
		delete(l.panes, pane)
	}
}

// current reports whether a message from loop gen of pane is from the loop
// running now.
func (l *loops) current(pane string, gen uint64) bool {
	p, ok := l.panes[pane]
	return ok && p.gen == gen
}

// followAgents makes the agent status loops match panes: one per agent pane.
func (l *loops) followAgents(panes []string) {
	for pane, cancel := range l.agents {
		if !slices.Contains(panes, pane) {
			cancel()
			delete(l.agents, pane)
		}
	}
	for _, pane := range panes {
		if _, ok := l.agents[pane]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(l.ctx)
		l.agents[pane] = cancel
		l.goLoop("the status loop for agent "+pane, func() { l.runAgentStatus(ctx, pane) })
	}
}

// tickMsg moves the engine's timers on.
type tickMsg struct{}

func (l *loops) runTick() {
	t := time.NewTicker(l.every.Tick)
	defer t.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
			l.deliver(l.ctx, tickMsg{})
		}
	}
}

// snapshotMsg is a fresh look at herdr's panes, agents and tabs.
type snapshotMsg struct {
	snap herdr.AgentSnapshot
	err  error
}

func (l *loops) runSync() {
	t := time.NewTicker(l.every.Sync)
	defer t.Stop()
	for {
		l.deliver(l.ctx, l.snapshot())
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
		case <-l.resync:
		}
	}
}

// snapshot fetches the three lists Sync needs. Panes are the one that matters:
// without them nothing can be chosen. Agents and tabs only add names, the state
// change counter and tab numbers, so if they fail the snapshot goes ahead.
func (l *loops) snapshot() snapshotMsg {
	snap := herdr.AgentSnapshot{Taken: l.now()}
	panes, err := l.client.ListPanes(l.ctx)
	if err != nil {
		return snapshotMsg{err: err}
	}
	snap.Panes = panes
	if agents, err := l.client.ListAgents(l.ctx); err == nil {
		snap.Agents = agents
	}
	if tabs, err := l.client.ListTabs(l.ctx); err == nil {
		snap.Tabs = tabs
	}
	return snapshotMsg{snap: snap}
}

// noteMove asks the watch list loop to put to in place of from in the file. It
// never blocks, because the loop may be waiting on the model to take a message.
func (l *loops) noteMove(from, to string) {
	l.movesMu.Lock()
	l.moves = append(l.moves, paneMove{from, to})
	l.movesMu.Unlock()
	select {
	case l.moved <- struct{}{}:
	default:
	}
}

// takeMoves empties the queue noteMove fills.
func (l *loops) takeMoves() []paneMove {
	l.movesMu.Lock()
	defer l.movesMu.Unlock()
	moves := l.moves
	l.moves = nil
	return moves
}

// watchListMsg is the watch list, sent when it changes and once at the start.
type watchListMsg struct {
	panes []string
	err   error
}

func (l *loops) runWatchList(dir string, extra []string) {
	t := time.NewTicker(l.every.List)
	defer t.Stop()
	var last watchListMsg
	first := true
	for {
		msg := watchListMsg{panes: slices.Clone(extra)}
		if dir != "" {
			panes, err := watchlist.Read(dir)
			msg.err = err
			for _, p := range panes {
				if !slices.Contains(msg.panes, p) {
					msg.panes = append(msg.panes, p)
				}
			}
		}
		if first || !sameList(msg, last) {
			first = false
			last = msg
			l.deliver(l.ctx, msg)
		}
		select {
		case <-l.ctx.Done():
			// A move just before quitting still reaches the file, or the
			// next feed would find the old id gone and drop the pane.
			l.saveMoves(dir)
			return
		case <-t.C:
		case <-l.moved:
			l.saveMoves(dir)
		}
	}
}

// saveMoves writes waiting pane moves to the watch list file, in the order
// they happened, so a pane that moves twice ends up under its last id. The
// model already follows each move, so a write that fails only means unwatch
// won't find the pane by its new id. That's reported and not retried.
func (l *loops) saveMoves(dir string) {
	for _, mv := range l.takeMoves() {
		if dir == "" {
			continue
		}
		if _, err := watchlist.Replace(dir, mv.from, mv.to); err != nil {
			l.deliver(l.ctx, moveSaveFailedMsg{pane: mv.to, err: err})
		}
	}
}

// moveSaveFailedMsg says a moved pane's new id couldn't be written to the
// watch list file.
type moveSaveFailedMsg struct {
	pane string
	err  error
}

func sameList(a, b watchListMsg) bool {
	errText := func(err error) string {
		if err == nil {
			return ""
		}
		return err.Error()
	}
	return slices.Equal(a.panes, b.panes) && errText(a.err) == errText(b.err)
}

// configMsg is sightline.toml, loaded again after a change.
type configMsg struct{ update config.Update }

// runConfig forwards config updates. It reads until config.Watch closes the
// channel, which Watch does as its last act once the root context ends, so
// waiting for this loop also waits for Watch's goroutine.
func (l *loops) runConfig(updates <-chan config.Update) {
	for u := range updates {
		l.deliver(l.ctx, configMsg{u})
	}
}

// eventMsg is one event from herdr.
type eventMsg struct {
	ev herdr.Event
	// global says it came from the subscription to pane events, not one
	// agent's status.
	global bool
}

// eventsDownMsg says a subscription couldn't be opened, or herdr refused to
// resume it, so some events won't arrive.
type eventsDownMsg struct {
	what string
	err  error
}

// agentDetected subscribes to herdr noticing an agent start in a pane. The herdr
// package has no helper for it, and the feed is the only thing that needs it.
var agentDetected = herdr.Subscription{Type: "pane.agent_detected"}

// runGlobalEvents follows panes closing, exiting and moving, and agents being
// detected. These don't name a pane, so a closed pane can't take them down.
//
// The client reconnects a running subscription by itself but doesn't retry the
// first subscribe, or one herdr refused, so this loop does, backing off up to
// 30 seconds. Without these events a moved pane would never be followed.
func (l *loops) runGlobalEvents() {
	const most = 30 * time.Second
	wait := time.Second
	for {
		events, err := l.client.Subscribe(l.ctx, herdr.PaneClosed(), herdr.PaneExited(), herdr.PaneMoved(), agentDetected)
		if err == nil {
			wait = time.Second
			l.deliver(l.ctx, eventsUpMsg{})
			for ev := range events {
				l.deliver(l.ctx, eventMsg{ev: ev, global: true})
				if ev.Kind == herdr.EventFailed {
					err = ev.Err
				}
			}
		}
		if l.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("the subscription ended")
		}
		l.deliver(l.ctx, eventsDownMsg{what: "pane events", err: err})
		t := time.NewTimer(wait)
		select {
		case <-l.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = min(wait*2, most)
	}
}

// eventsUpMsg says the global subscription is running.
type eventsUpMsg struct{}

// runAgentStatus follows one agent pane's status changes. herdr only offers
// them per pane. The tracker works activity out from snapshots as well, so a
// subscription that fails costs speed, not correctness, and isn't reported.
func (l *loops) runAgentStatus(ctx context.Context, pane string) {
	events, err := l.client.Subscribe(ctx, herdr.AgentStatusChanged(pane))
	if err != nil {
		return
	}
	for ev := range events {
		l.deliver(ctx, eventMsg{ev: ev})
	}
}

// deltaMsg is what one read of a watched pane found.
type deltaMsg struct {
	pane  string
	gen   uint64
	delta herdr.Delta
	// hintsLost means the pane's scroll subscription dropped and came back, so
	// hints may have been missed and a quiet read can't be trusted.
	hintsLost bool
}

// paneGoneMsg says a watched pane has closed.
type paneGoneMsg struct {
	pane string
	gen  uint64
}

// paneErrMsg says reading a watched pane failed for a reason other than it
// closing. The loop keeps trying.
type paneErrMsg struct {
	pane string
	gen  uint64
	err  error
}

// runPane owns one pane's PaneWatcher. It reads the pane every Poll, and sooner
// when a scroll hint says the pane printed something, and sends what each read
// found to the model. It stops when ctx ends or the pane closes.
func (l *loops) runPane(ctx context.Context, pane string, gen uint64) {
	w := herdr.NewPaneWatcher(l.client, pane)

	// Scroll hints are a bonus. Without them reads still happen on time, they
	// just can't tell a quiet pane from a full window of the same output.
	hints, err := l.client.Subscribe(ctx, herdr.ScrollChanged(pane))
	switch {
	case herdr.IsPaneNotFound(err):
		l.deliver(ctx, paneGoneMsg{pane: pane, gen: gen})
		return
	case err != nil:
		l.deliver(ctx, paneErrMsg{pane: pane, gen: gen, err: err})
		hints = nil
	}

	t := time.NewTicker(l.every.Poll)
	defer t.Stop()
	var lastRead time.Time
	read := func(hintsLost bool) bool {
		lastRead = time.Now()
		d, err := w.Poll(ctx)
		switch {
		case ctx.Err() != nil:
			return false
		case herdr.IsPaneNotFound(err):
			l.deliver(ctx, paneGoneMsg{pane: pane, gen: gen})
			return false
		case err != nil:
			l.deliver(ctx, paneErrMsg{pane: pane, gen: gen, err: err})
			return true
		}
		l.deliver(ctx, deltaMsg{pane: pane, gen: gen, delta: d, hintsLost: hintsLost})
		return true
	}

	if !read(false) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !read(false) {
				return
			}
		case ev, ok := <-hints:
			if !ok {
				hints = nil
				continue
			}
			switch ev.Kind {
			case herdr.EventScrollChanged:
				if ev.Scroll == nil || !w.Hint(*ev.Scroll) || time.Since(lastRead) < l.every.EarlyRead {
					continue
				}
				if !read(false) {
					return
				}
				t.Reset(l.every.Poll)
			case herdr.EventReconnected:
				if !read(true) {
					return
				}
			}
		}
	}
}

// faults stops the feed on the first panic in any of its goroutines and keeps
// that panic for Run to return. Later panics are most likely knock-on effects
// of the first, so only the first is kept.
type faults struct {
	ctx    context.Context    // the root, which carries a test's panic hook
	cancel context.CancelFunc // cancels the root

	mu    sync.Mutex
	first error
}

// fail keeps err if it's the first and stops the feed.
func (f *faults) fail(err error) {
	f.mu.Lock()
	if f.first == nil {
		f.first = err
	}
	f.mu.Unlock()
	f.cancel()
}

// err is the first panic, or nil if nothing has panicked.
func (f *faults) err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.first
}

// catch recovers a panic and fails the feed with it. It only works deferred
// directly, as in defer f.catch("the tick loop"), because that's the only way
// recover sees the panic.
func (f *faults) catch(where string) {
	if r := recover(); r != nil {
		f.fail(newPanicError(where, r))
	}
}

// guard wraps a Bubble Tea command so a panic in it fails the feed rather than
// reaching Bubble Tea. The command then gives back no message. It doesn't reach
// inside tea.Batch or tea.Sequence, whose commands Bubble Tea runs itself, so
// the feed doesn't use them.
func (f *faults) guard(where string, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		defer f.catch(where)
		panicPoint(f.ctx, where)
		return cmd()
	}
}

// panicError is a recovered panic: where it happened, what it was and the
// stack at that moment.
type panicError struct {
	where string
	value any
	stack []byte
}

// newPanicError builds a panicError. Call it from the deferred function that
// recovered, while the panicking frames are still on the stack.
func newPanicError(where string, value any) *panicError {
	return &panicError{where: where, value: value, stack: debug.Stack()}
}

func (e *panicError) Error() string {
	// The value could hold pane text, and this ends up on the terminal once
	// the feed has stopped, so it's cleaned like anything else from a pane.
	return fmt.Sprintf("%s panicked: %s\n\n%s", clean(e.where), clean(fmt.Sprint(e.value)), e.stack)
}

// panicError deliberately has no Unwrap. A panic with a *herdr.Error in it
// would otherwise look like herdr refusing a prompt, which the model reads as
// nothing typed, when a panic during a send says nothing either way.

// panicKey is the context key for a test's panic hook.
type panicKey struct{}

// panicPoint is where a test can make the feed panic. It calls the hook a test
// put in the context, if there is one, with where it is. Outside tests there's
// never a hook, so it does nothing.
func panicPoint(ctx context.Context, where string) {
	if hook, ok := ctx.Value(panicKey{}).(func(string)); ok {
		hook(where)
	}
}
