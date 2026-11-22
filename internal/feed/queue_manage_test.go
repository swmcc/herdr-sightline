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

// v on a queued entry that would go with others shows that batch, exactly as
// queue mode then sends it (#92).
func TestVShowsTheBatchBeforeItGoes(t *testing.T) {
	h := newHarness(t)
	older, newer := h.queueTwo()
	h.press("v")
	if h.model.mode != modeView || h.model.viewBatch == nil {
		t.Fatalf("v on a queued entry didn't show its batch, status %q", h.model.status)
	}
	want, err := h.model.engine.PreviewBatch([]engine.EntryID{older.ID, newer.ID})
	if err != nil {
		t.Fatal(err)
	}
	if h.model.viewText != want || !strings.HasPrefix(want, "Sightline caught 2 errors in pane w1:p1.") {
		t.Fatalf("the view shows\n%s\nwant the batch\n%s", h.model.viewText, want)
	}
	if view := h.model.View(); !strings.Contains(view, "The batch queue mode will send to claude (tab 2) once it can, queued 1 and 2 of 2") {
		t.Errorf("the header doesn't say what the batch is:\n%s", view)
	}
	if h.press("enter") != nil || len(h.herdr.promptCalls()) != 0 {
		t.Fatal("enter on the batch sent something")
	}

	// Another error joins the batch while it's on screen. The view follows,
	// so it's never out of date when queue mode sends.
	h.printAndRun("RangeError: Maximum call stack size exceeded")
	if !strings.HasPrefix(h.model.viewText, "Sightline caught 3 errors in pane w1:p1.") || !strings.Contains(h.model.viewText, "RangeError") {
		t.Fatalf("the view didn't follow the batch:\n%s", h.model.viewText)
	}
	if !strings.Contains(h.model.status, "The batch changed") || !strings.Contains(h.model.View(), "queued 1, 2 and 3 of 3") {
		t.Errorf("status = %q, want it to say the batch changed:\n%s", h.model.status, h.model.View())
	}
	shown := h.model.viewText

	h.herdr.setStatus(agentPane, herdr.AgentIdle)
	h.sync()
	h.clock.Advance(time.Minute)
	h.tickAndRun()
	calls := h.herdr.promptCalls()
	if len(calls) != 1 || calls[0].Text != shown {
		t.Fatalf("queue mode sent %+v, want exactly what v showed:\n%s", calls, shown)
	}
	// Once it has gone, the view shows what went.
	h.tickAndRun()
	if !h.model.viewSent || h.model.viewText != shown {
		t.Errorf("after the send the view shows\n%s", h.model.View())
	}
}

// A queued entry that would go on its own shows what enter sends, which is
// the same bytes.
func TestVOnALoneQueuedEntryShowsWhatEnterSends(t *testing.T) {
	h := newHarness(t)
	h.herdr.setStatus(agentPane, herdr.AgentWorking)
	en := h.queueOn()
	h.press("v")
	want, err := h.model.engine.Message(en.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h.model.viewBatch != nil || h.model.viewText != want {
		t.Errorf("v on a lone queued entry:\n%s", h.model.View())
	}
	if preview, err := h.model.engine.PreviewBatch([]engine.EntryID{en.ID}); err != nil || preview != want {
		t.Errorf("a batch of one isn't what enter sends: %v", err)
	}
}
