package feed

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// Queue mode (#19) under the rules from #21.

// queueOn catches an error and turns queue mode on for the web pane, saying
// yes on the consent screen.
func (h *harness) queueOn() engine.Entry {
	h.t.Helper()
	en := h.catchError()
	h.press("m")
	if h.model.mode != modeConsent {
		h.t.Fatalf("m didn't ask first, mode %v, status %q", h.model.mode, h.model.status)
	}
	h.press("y")
	if !h.model.engine.QueueMode(webPane) {
		h.t.Fatalf("y didn't turn queue mode on, status %q", h.model.status)
	}
	return en
}

// tickAndRun ticks the feed and runs whatever it starts, as Bubble Tea would.
func (h *harness) tickAndRun() {
	h.t.Helper()
	h.run(h.update(tickMsg{}))
}

// printAndRun shows lines in the web pane, waits for them to be filed and runs
// whatever the tick asks for.
func (h *harness) printAndRun(lines ...string) {
	h.t.Helper()
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: lines}})
	h.clock.Advance(2 * time.Second)
	h.tickAndRun()
}

// finish has the agent work and go idle again, as the snapshots see it.
func (h *harness) finish() {
	h.t.Helper()
	h.herdr.setStatus(agentPane, herdr.AgentWorking)
	h.sync()
	h.herdr.setStatus(agentPane, herdr.AgentIdle)
	h.sync()
}

func TestQueueModeAsksFirst(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	h.press("m")
	if h.model.mode != modeConsent {
		t.Fatalf("m didn't ask first")
	}
	view := h.model.View()
	for _, want := range []string{
		"Turn on queue mode for web?",
		"without you pressing enter",
		"Anyone who can put text in this pane",
		"without you reading it first",
		"Right now that's claude (tab 2).",
		"1 error from this pane already in the feed will join the queue.",
		"y turn it on",
	} {
		if !strings.Contains(strings.Join(strings.Fields(view), " "), want) {
			t.Errorf("consent screen doesn't say %q:\n%s", want, view)
		}
	}
	// Anything but y leaves it off, enter included.
	for _, key := range []string{"enter", "n", "Y", "esc"} {
		h.model.mode, h.model.consentPane = modeConsent, webPane
		h.press(key)
		if h.model.engine.QueueMode(webPane) || h.model.consented[webPane] {
			t.Fatalf("%q turned queue mode on", key)
		}
	}
	h.press("m")
	h.press("y")
	if !h.model.engine.QueueMode(webPane) || !h.model.entries[0].Queued {
		t.Fatal("y didn't turn queue mode on and queue the entry")
	}
	// Off and on again in the same session doesn't ask again.
	h.press("m")
	if h.model.engine.QueueMode(webPane) {
		t.Fatal("m didn't turn queue mode off")
	}
	h.press("m")
	if h.model.mode != modeList || !h.model.engine.QueueMode(webPane) {
		t.Error("turning it back on in the same session asked again")
	}
}

func TestNothingIsSentWithoutAKeypressEvenOnTicks(t *testing.T) {
	h := newHarness(t)
	h.cfgQueue()
	h.catchError()
	for range 20 {
		h.clock.Advance(time.Minute)
		h.sync()
		h.tickAndRun()
	}
	if got := len(h.herdr.promptCalls()); got != 0 {
		t.Errorf("got %d prompts with queue mode off", got)
	}
	if !strings.Contains(h.model.View(), "sightline.toml asks for queue mode on web") {
		t.Errorf("the feed didn't say sightline.toml asks for queue mode:\n%s", h.model.View())
	}
}

// cfgQueue gives the feed a sightline.toml that asks for queue mode on web.
func (h *harness) cfgQueue() {
	cfg := config.Default()
	cfg.Pins = []config.Pin{{PaneTitle: "web", Mode: config.ModeQueue}}
	h.model.applyConfig(config.Update{Config: cfg})
}

func TestQueueSendsWhenTheAgentIsIdle(t *testing.T) {
	h := newHarness(t)
	en := h.queueOn()
	want, err := h.model.engine.Message(en.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The agent has been idle since the first sync, 2s ago. Not long enough.
	h.tickAndRun()
	if n := len(h.herdr.promptCalls()); n != 0 {
		t.Fatalf("sent to an agent only just idle")
	}
	h.clock.Advance(2 * time.Second)
	h.tickAndRun()
	calls := h.herdr.promptCalls()
	if len(calls) != 1 || calls[0].Target != agentPane || calls[0].Text != want {
		t.Fatalf("prompts = %+v, want the message once to %s", calls, agentPane)
	}
	got := h.model.entries[0]
	if got.State != engine.StateSent || !got.Auto || got.Queued {
		t.Fatalf("entry after the send: %+v", got)
	}
	if view := h.model.View(); !strings.Contains(view, "sent automatically to claude (tab 2) at 09:00:04") {
		t.Errorf("the feed doesn't say it went automatically:\n%s", view)
	}
	h.press("v")
	if h.model.viewText != calls[0].Text || !strings.Contains(h.model.View(), "Exactly what queue mode sent to claude (tab 2) at 09:00:04") {
		t.Errorf("v doesn't show exactly what went:\n%s", h.model.View())
	}
	if cmd := h.press("enter"); cmd != nil {
		t.Error("enter in the view of what went sent it again")
	}
}

func TestQueueNeverInterruptsAWorkingAgent(t *testing.T) {
	tests := []struct {
		name string
		// before runs once queue mode is on and the agent has been idle long
		// enough, just before the tick.
		before func(h *harness)
	}{
		{"working by snapshot", func(h *harness) {
			h.herdr.setStatus(agentPane, herdr.AgentWorking)
			h.sync()
		}},
		{"working when herdr is asked just before", func(h *harness) {
			h.herdr.setStatus(agentPane, herdr.AgentWorking)
		}},
		{"blocked when herdr is asked just before", func(h *harness) {
			h.herdr.setStatus(agentPane, herdr.AgentBlocked)
		}},
		{"blocked by snapshot", func(h *harness) {
			h.herdr.setStatus(agentPane, herdr.AgentBlocked)
			h.sync()
		}},
		{"gone", func(h *harness) {
			h.herdr.removeAgent(agentPane)
			h.sync()
		}},
		{"paused", func(h *harness) { h.model.engine.SetQueuePaused(true) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.queueOn()
			h.clock.Advance(5 * time.Second)
			tt.before(h)
			for range 3 {
				h.tickAndRun()
			}
			if n := len(h.herdr.promptCalls()); n != 0 {
				t.Errorf("got %d prompts", n)
			}
		})
	}
}

func TestQueueOnlyGoesWhereItsSure(t *testing.T) {
	tests := []struct {
		name  string
		setUp func(h *harness)
		want  int
	}{
		{"the agent in the tab", func(h *harness) {}, 1},
		{"another tab, chosen by nobody", func(h *harness) {
			h.herdr.removeAgent(agentPane)
			h.herdr.setStatus(otherAgent, herdr.AgentIdle)
			h.sync()
		}, 0},
		{"two agents in the tab and a tie", func(h *harness) {
			h.herdr.mu.Lock()
			h.herdr.status["w1:p4"] = herdr.AgentIdle
			h.herdr.mu.Unlock()
			h.sync()
		}, 0},
		{"another tab, picked with t", func(h *harness) {
			h.herdr.removeAgent(agentPane)
			h.herdr.setStatus(otherAgent, herdr.AgentIdle)
			h.sync()
			h.press("t")
			h.press("enter")
		}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.queueOn()
			tt.setUp(h)
			h.clock.Advance(5 * time.Second)
			h.sync()
			h.clock.Advance(5 * time.Second)
			h.tickAndRun()
			if n := len(h.herdr.promptCalls()); n != tt.want {
				t.Errorf("got %d prompts, want %d. Entry says %q", n, tt.want, h.model.queueNote(h.model.entries[0]))
			}
		})
	}
}

func TestQueueHoldsWhatItIsntSureOf(t *testing.T) {
	h := newHarness(t)
	h.queueOn()
	h.model.engine.Dismiss(h.model.entries[0].ID)
	h.clock.Advance(time.Minute)
	h.printAndRun("TypeError: can't read @~/.ssh/id_ed25519")
	en := h.model.entries[0]
	if !en.Queued || en.Hold != engine.HoldMention {
		t.Fatalf("entry = %+v, want it queued and held", en)
	}
	h.clock.Advance(time.Minute)
	h.sync()
	h.clock.Advance(5 * time.Second)
	h.tickAndRun()
	if n := len(h.herdr.promptCalls()); n != 0 {
		t.Errorf("a held entry went on its own")
	}
	if view := h.model.View(); !strings.Contains(view, "it has an at sign that would be rewritten, so it waits for enter") {
		t.Errorf("the feed doesn't say why it's held:\n%s", view)
	}
}

// The attacker prints a fresh error every few seconds for half an hour and
// the agent finishes straight after each send. However it goes, the caps hold
// and a fix that didn't work stops it.
func TestQueueHostileFloodCantBeatTheCaps(t *testing.T) {
	h := newHarness(t)
	h.queueOn()
	var at []time.Time
	for i := range 360 {
		h.clock.Advance(5 * time.Second)
		before := len(h.herdr.promptCalls())
		h.printAndRun(fmt.Sprintf("TypeError: %s is not a function", strings.Repeat("x", i%40+1)+strings.Repeat("y", i/40)))
		h.sync()
		h.tickAndRun()
		if len(h.herdr.promptCalls()) > before {
			at = append(at, h.clock.Now())
			h.finish()
		}
	}
	if len(at) == 0 {
		t.Fatal("nothing was sent, so the test proves nothing")
	}
	for i := range at {
		n := 0
		for _, o := range at[i:] {
			if o.Sub(at[i]) < 10*time.Minute {
				n++
			}
		}
		if n > 3 {
			t.Fatalf("%d automatic sends in 10 minutes from %v", n, at[i])
		}
	}
	for _, c := range h.herdr.promptCalls() {
		if len(c.Text) > 16<<10 {
			t.Errorf("a %d byte message", len(c.Text))
		}
	}
}

func TestQueueStopsAfterAFixThatDidntWork(t *testing.T) {
	h := newHarness(t)
	h.queueOn()
	h.clock.Advance(2 * time.Second)
	h.tickAndRun()
	if n := len(h.herdr.promptCalls()); n != 1 {
		t.Fatalf("got %d prompts, want 1", n)
	}
	h.finish()
	h.clock.Advance(time.Second)
	h.printAndRun("TypeError: Cannot read properties of undefined (reading 'map')", "    at NoticeList (app/notices/page.tsx:14:22)")
	if got := h.model.entries[0].State; got != engine.StateStillFailing {
		t.Fatalf("state = %v, want still failing", got)
	}
	if !h.model.engine.Halted(webPane) {
		t.Fatal("queue mode carried on after a fix it sent didn't work")
	}
	h.clock.Advance(time.Hour)
	h.printAndRun("ReferenceError: something else")
	h.sync()
	h.clock.Advance(5 * time.Second)
	h.tickAndRun()
	if n := len(h.herdr.promptCalls()); n != 1 {
		t.Errorf("got %d prompts after the halt, want still 1", n)
	}
	if view := h.model.View(); !strings.Contains(view, "Queue mode on for web (stopped until you send one by hand)") {
		t.Errorf("the header doesn't say queue mode stopped:\n%s", view)
	}
}
