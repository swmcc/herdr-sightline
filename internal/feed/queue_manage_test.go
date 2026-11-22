package feed

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// Managing the queue from the feed (#20).

// queueTwo turns queue mode on with two entries queued and the agent working,
// so nothing goes until a test lets it. The newest entry is selected.
func (h *harness) queueTwo() (older, newer engine.Entry) {
	h.t.Helper()
	h.herdr.setStatus(agentPane, herdr.AgentWorking)
	older = h.queueOn()
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: []string{"ReferenceError: user is not defined"}}})
	h.clock.Advance(2 * time.Second)
	h.sync()
	h.tickAndRun()
	if len(h.model.engine.Queue()) != 2 {
		h.t.Fatalf("queued %v, want two entries", h.model.engine.Queue())
	}
	newer = h.model.entries[0]
	h.model.selected = newer.ID
	return older, newer
}

func TestXTakesAnEntryOutOfTheQueue(t *testing.T) {
	h := newHarness(t)
	older, newer := h.queueTwo()
	h.press("x")
	if q := h.model.engine.Queue(); !slices.Equal(q, []engine.EntryID{older.ID}) {
		t.Fatalf("queue = %v, want only %s", q, older.ID)
	}
	if en, _ := h.model.entry(newer.ID); en.Queued || en.State != engine.StateNew {
		t.Errorf("after x: %+v, want it new and out of the queue", en)
	}
	// It never goes on its own after that.
	h.herdr.setStatus(agentPane, herdr.AgentIdle)
	h.sync()
	for range 4 {
		h.clock.Advance(time.Minute)
		h.tickAndRun()
		h.finish()
	}
	for _, c := range h.herdr.promptCalls() {
		if strings.Contains(c.Text, "user is not defined") {
			t.Errorf("an entry taken out of the queue went on its own")
		}
	}
}

func TestBracketsMoveAQueuedEntry(t *testing.T) {
	h := newHarness(t)
	older, newer := h.queueTwo()
	if !strings.Contains(h.model.View(), "◷ 2 of 2 queued for claude (tab 2)") {
		t.Errorf("the entry doesn't say where it is in the queue:\n%s", h.model.View())
	}
	h.press("[")
	if q := h.model.engine.Queue(); !slices.Equal(q, []engine.EntryID{newer.ID, older.ID}) {
		t.Fatalf("queue = %v after [", q)
	}
	h.press("[")
	if !strings.Contains(h.model.status, "can't move") {
		t.Errorf("status = %q at the front", h.model.status)
	}
	h.press("]")
	if q := h.model.engine.Queue(); !slices.Equal(q, []engine.EntryID{older.ID, newer.ID}) {
		t.Errorf("queue = %v after ]", q)
	}
}

func TestPPausesQueueMode(t *testing.T) {
	h := newHarness(t)
	h.queueTwo()
	h.press("s")
	if !strings.Contains(h.model.View(), "paused, press s to carry on") {
		t.Errorf("the header doesn't say it's paused:\n%s", h.model.View())
	}
	h.herdr.setStatus(agentPane, herdr.AgentIdle)
	h.sync()
	h.clock.Advance(time.Minute)
	h.tickAndRun()
	if n := len(h.herdr.promptCalls()); n != 0 {
		t.Fatalf("got %d prompts while paused", n)
	}
	h.press("s")
	h.tickAndRun()
	calls := h.herdr.promptCalls()
	if len(calls) != 1 || !strings.HasPrefix(calls[0].Text, "Sightline caught 2 errors in pane w1:p1.") {
		t.Fatalf("after carrying on: %+v, want one message with both errors", calls)
	}
}

func TestALogShowsWhatWent(t *testing.T) {
	h := newHarness(t)
	h.press("a")
	if h.model.mode != modeList {
		t.Fatal("a opened an empty log")
	}
	h.queueTwo()
	h.herdr.setStatus(agentPane, herdr.AgentIdle)
	h.sync()
	h.clock.Advance(time.Minute)
	h.tickAndRun()
	calls := h.herdr.promptCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d prompts", len(calls))
	}
	h.press("a")
	view := h.model.View()
	if !strings.Contains(view, "2 errors to claude (tab 2) from web") {
		t.Errorf("the log doesn't list the send:\n%s", view)
	}
	h.press("enter")
	if h.model.viewText != calls[0].Text || !h.model.viewSent {
		t.Errorf("enter in the log doesn't show exactly what went")
	}
	if h.press("enter") != nil {
		t.Error("enter on what went sent something")
	}
}
