package injection

import (
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// Fixed must stay out of an attacker's reach as much as the message format is.
// An attacker who can print into the pane can print a line that looks like a
// recompile, but that alone mustn't turn an error that may still be there into
// "fixed".

const (
	realError = "TypeError: Cannot read properties of undefined (reading 'map')"
	recompile = "compiled successfully in 80 ms"
)

// settleWith catches realError, sends it, has the agent finish and then lets
// attack print during the settle period. It returns the entry's state once the
// verdict is in.
func settleWith(t *testing.T, attack func(e *engine.Engine, now time.Time)) engine.State {
	t.Helper()
	e := engine.New(options(t))
	e.Watch(engine.Source{ID: paneID})
	now := start
	e.Feed(paneID, []string{realError}, now)
	now = now.Add(2 * time.Second)
	e.Tick(now)
	entries := e.Entries()
	if len(entries) != 1 {
		t.Fatalf("want the error caught once, got %+v", entries)
	}
	id := entries[0].ID
	e.MarkSent(id, "claude", now)
	now = now.Add(5 * time.Second)
	e.AgentFinished("claude", now)
	attack(e, now.Add(time.Second))
	e.Tick(now.Add(2 * time.Minute))
	for _, en := range e.Entries() {
		if en.ID == id {
			return en.State
		}
	}
	t.Fatalf("entry %s is gone", id)
	return 0
}

func TestAttackerCantMakeAnErrorFixed(t *testing.T) {
	// The control: an honest recompile and no error is fixed, so the cases
	// below are really being kept from it.
	if got := settleWith(t, func(e *engine.Engine, now time.Time) {
		e.Feed(paneID, []string{recompile}, now)
	}); got != engine.StateFixed {
		t.Fatalf("honest recompile gave %v, want fixed", got)
	}

	tests := []struct {
		name   string
		attack func(e *engine.Engine, now time.Time)
	}{
		{
			// A request log that opens an OSC and never closes it hides the
			// real error coming back, from the person and from Sightline. Then
			// the string closes and a fake recompile follows.
			"error hidden in a string, then a fake recompile",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{"GET /\x1b]0;", realError, "    at NoticeList (app/notices/page.tsx:14:22)", "\x07" + recompile}, now)
			},
		},
		{
			"fake recompile in a read that may have missed output",
			func(e *engine.Engine, now time.Time) {
				e.Gap(paneID, now)
				e.Feed(paneID, []string{recompile}, now)
			},
		},
		{
			"fake recompile in a read that can't say what's new",
			func(e *engine.Engine, now time.Time) {
				e.Unsure(paneID, now)
				e.Feed(paneID, []string{recompile}, now)
			},
		},
		{
			"fake recompile the person can't see, in a title",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{"\x1b]0;" + recompile + "\x07"}, now)
			},
		},
		{
			"fake recompile drawn over",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{recompile + "\r\x1b[2KGET /health 200"}, now)
			},
		},
		{
			// Anyone who can reach the dev server can put words in its request
			// log.
			"fake recompile in a request path",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{"GET /compiled/successfully 404 in 3ms"}, now)
			},
		},
		{
			"fake build in a request path",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{`127.0.0.1 - - "GET /x?built%20in%201s&ready in 3ms HTTP/1.1" 404 0`}, now)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := settleWith(t, tt.attack); got == engine.StateFixed {
				t.Fatalf("an attacker made the error read as fixed")
			}
		})
	}
}

// Through herdr, a burst bigger than the read with a fake recompile in it comes
// back as a gap, so it can't make the error fixed either.
func TestFakeRecompileInAFloodIsntFixed(t *testing.T) {
	pane := &fakePane{}
	w := herdr.NewPaneWatcher(pane, paneID)
	w.Lines = 50
	e := engine.New(options(t))
	e.Watch(engine.Source{ID: paneID})
	now := start
	adapterPoll(t, w, e, now)
	pane.lines = append(pane.lines, realError)
	adapterPoll(t, w, e, now)
	now = now.Add(2 * time.Second)
	adapterPoll(t, w, e, now)
	id := e.Entries()[0].ID
	e.MarkSent(id, "claude", now)
	now = now.Add(time.Second)
	e.AgentFinished("claude", now)

	now = now.Add(time.Second)
	for i := range 200 {
		pane.lines = append(pane.lines, "GET /health 200 "+string(rune('a'+i%26)))
	}
	pane.lines = append(pane.lines, recompile)
	adapterPoll(t, w, e, now)
	for range 40 {
		now = now.Add(time.Second)
		adapterPoll(t, w, e, now)
	}
	if got := e.Entries()[0].State; got == engine.StateFixed {
		t.Fatal("a fake recompile in a flood made the error read as fixed")
	}
}
