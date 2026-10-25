// Package enginetest is a fake adapter for testing the engine without herdr.
//
// It plays a script of output against an engine with a fake clock, so tests can
// cover the capture wait, the cooldown and the settle period without sleeping.
package enginetest

import (
	"fmt"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
)

// Start is the fake clock's starting time. Any fixed time would do. It's a
// function so no test can change it under another.
func Start() time.Time { return time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC) }

// Step is one scripted event. The clock moves on by Wait first, then the step
// happens and the engine is ticked.
type Step struct {
	Wait   time.Duration
	Source engine.SourceID
	Lines  []string // printed by Source
	Gap    bool     // output from Source may have been missed
}

// Adapter drives an engine the way the herdr adapter would, from one goroutine.
type Adapter struct {
	Engine *engine.Engine
	now    time.Time
}

// New makes a fake adapter around a new engine built from opts.
func New(opts engine.Options) *Adapter {
	return &Adapter{Engine: engine.New(opts), now: Start()}
}

// Now is the fake clock's current time.
func (a *Adapter) Now() time.Time { return a.now }

// Watch starts watching a source with the given id, title and command.
func (a *Adapter) Watch(id engine.SourceID, title, command string) {
	a.Engine.Watch(engine.Source{ID: id, Title: title, Command: command})
}

// Print feeds lines from id at the current time.
func (a *Adapter) Print(id engine.SourceID, lines ...string) {
	a.Engine.Feed(id, lines, a.now)
}

// Advance moves the clock on by d and ticks the engine.
func (a *Adapter) Advance(d time.Duration) {
	a.now = a.now.Add(d)
	a.Engine.Tick(a.now)
}

// Run plays a script.
func (a *Adapter) Run(script []Step) {
	for _, s := range script {
		a.now = a.now.Add(s.Wait)
		if s.Gap {
			a.Engine.Gap(s.Source, a.now)
		}
		if len(s.Lines) > 0 {
			a.Engine.Feed(s.Source, s.Lines, a.now)
		}
		a.Engine.Tick(a.now)
	}
}

// Send marks an entry sent to agent at the current time.
func (a *Adapter) Send(id engine.EntryID, agent string) {
	a.Engine.MarkSent(id, agent, a.now)
}

// Finish says agent finished at the current time.
func (a *Adapter) Finish(agent string) {
	a.Engine.AgentFinished(agent, a.now)
}

// Only returns the one entry in the engine, or an error if there isn't exactly one.
func (a *Adapter) Only() (engine.Entry, error) {
	entries := a.Engine.Entries()
	if len(entries) != 1 {
		return engine.Entry{}, fmt.Errorf("want 1 entry, got %d: %+v", len(entries), entries)
	}
	return entries[0], nil
}
