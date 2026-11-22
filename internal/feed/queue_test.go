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
		"Turn on queue mode for web (pane w1:p1)?",
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

// Turning queue mode off, or dismissing, while the check before an automatic
// send is on its way stops it.
func TestQueueChangeOfMindDuringTheCheck(t *testing.T) {
	tests := []struct {
		name   string
		change func(h *harness)
	}{
		{"m turns it off", func(h *harness) { h.press("m") }},
		{"unwatched", func(h *harness) { h.update(watchListMsg{}) }},
		{"halted", func(h *harness) { h.model.engine.Halt(webPane) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.queueOn()
			h.clock.Advance(5 * time.Second)
			cmd := h.update(tickMsg{})
			if cmd == nil {
				t.Fatal("the tick started no send")
			}
			tt.change(h)
			h.run(cmd)
			if n := len(h.herdr.promptCalls()); n != 0 {
				t.Errorf("got %d prompts after a change of mind", n)
			}
			if h.model.autoBusy || len(h.model.sending) != 0 {
				t.Errorf("the queue is left jammed: busy %v, sending %v", h.model.autoBusy, h.model.sending)
			}
		})
	}
	// The agent did something during the check: its counter moved.
	h := newHarness(t)
	h.queueOn()
	h.clock.Advance(5 * time.Second)
	cmd := h.update(tickMsg{})
	h.herdr.mu.Lock()
	h.herdr.seq[agentPane]++
	h.herdr.mu.Unlock()
	h.run(cmd)
	if n := len(h.herdr.promptCalls()); n != 0 {
		t.Errorf("sent to an agent whose counter moved during the check")
	}

	// A keypress for the same agent while an automatic send is on its way
	// waits, so two prompts can't land before Sightline knows of the first.
	h = newHarness(t)
	first := h.queueOn()
	h.model.engine.Unqueue(first.ID)
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: []string{"ReferenceError: other is not defined"}}})
	h.clock.Advance(5 * time.Second)
	cmd = h.update(tickMsg{})
	if cmd == nil {
		t.Fatal("the tick started no send")
	}
	h.model.selected = first.ID
	if h.press("enter") != nil {
		t.Error("a keypress sent to the agent queue mode was sending to")
	}
	h.run(cmd)

	h = newHarness(t)
	h.queueOn()
	h.clock.Advance(5 * time.Second)
	cmd = h.update(tickMsg{})
	h.press("d")
	if !h.model.entries[0].Queued {
		t.Error("d dismissed an entry while its send was on its way")
	}
	h.run(cmd)
}

// An unlabelled pane sets its own title, and with it whether a pin matches.
// Queue mode doesn't let it choose: nothing goes on its own while it's titled
// like the pin, and nothing goes once it retitles itself out of the pin.
func TestQueueRefusesAPaneWhoseTitleDecidesThePin(t *testing.T) {
	h := newHarness(t)
	h.herdr.mu.Lock()
	h.herdr.webLabel, h.herdr.webTitle = "", "web"
	h.herdr.mu.Unlock()
	cfg := config.Default()
	cfg.Pins = []config.Pin{{PaneTitle: "web", Agent: "claude"}}
	h.model.applyConfig(config.Update{Config: cfg})
	h.queueOn()
	for _, title := range []string{"web", "x"} {
		h.herdr.mu.Lock()
		h.herdr.webTitle = title
		h.herdr.mu.Unlock()
		h.sync()
		h.clock.Advance(5 * time.Second)
		h.tickAndRun()
		if n := len(h.herdr.promptCalls()); n != 0 {
			t.Fatalf("titled %q: got %d prompts", title, n)
		}
	}
}

// Once an entry that went on its own has a verdict or fresh lines, v shows
// what enter would send now, not what went.
func TestViewAfterAnAutoSendShowsWhatEnterSends(t *testing.T) {
	h := newHarness(t)
	en := h.queueOn()
	h.clock.Advance(2 * time.Second)
	h.tickAndRun()
	h.finish()
	h.clock.Advance(time.Second)
	h.print(" GET /other 500", "TypeError: Cannot read properties of undefined (reading 'map')", "    at NoticeList (app/notices/page.tsx:14:22)")
	if got := h.model.entries[0].State; got != engine.StateStillFailing {
		t.Fatalf("state = %v, want still failing", got)
	}
	want, err := h.model.engine.Message(en.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.press("v")
	if h.model.viewSent || h.model.viewText != want {
		t.Errorf("v on a still failing entry doesn't show what enter sends:\n%s", h.model.View())
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

// A pin whose agent isn't running falls back to the tab and says so. That's not
// an agent Sightline is sure of, so queue mode waits for a keypress. A pin that
// matches a running agent still goes on its own, which shows the test can tell
// the two apart.
func TestQueueWaitsWhenAPinFellBackToTheTab(t *testing.T) {
	tests := []struct {
		agent string
		want  int
	}{
		{"codex", 0},
		{"claude", 1},
	}
	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			h := newHarness(t)
			cfg := config.Default()
			cfg.Pins = []config.Pin{{PaneTitle: "web", Agent: tt.agent}}
			h.model.applyConfig(config.Update{Config: cfg})
			h.queueOn()
			h.sync()
			h.clock.Advance(5 * time.Second)
			h.tickAndRun()
			if n := len(h.herdr.promptCalls()); n != tt.want {
				t.Errorf("pinned to %s: got %d prompts, want %d", tt.agent, n, tt.want)
			}
		})
	}
}

// You may be typing in the agent's pane when it goes idle, and herdr doesn't
// say whether a prompt clears a half typed draft. So nothing goes on its own
// while that pane has focus: not when the snapshot says so, and not when focus
// arrives between the snapshot and the check just before sending. Once focus
// moves away it goes.
func TestQueueWaitsWhileTheAgentPaneHasFocus(t *testing.T) {
	setFocus := func(h *harness, pane string) {
		h.herdr.mu.Lock()
		h.herdr.focused = pane
		h.herdr.mu.Unlock()
	}
	tests := []struct {
		name  string
		setUp func(h *harness)
		want  int
	}{
		{"focused in the snapshot", func(h *harness) { setFocus(h, agentPane); h.sync() }, 0},
		{"focused just before sending", func(h *harness) { setFocus(h, agentPane) }, 0},
		{"focus somewhere else", func(h *harness) { setFocus(h, webPane); h.sync() }, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.queueOn()
			tt.setUp(h)
			h.clock.Advance(5 * time.Second)
			h.tickAndRun()
			if n := len(h.herdr.promptCalls()); n != tt.want {
				t.Errorf("got %d prompts, want %d", n, tt.want)
			}
		})
	}
}

// An entry that went on its own keeps saying so, and when, after its verdict
// (#21, #93). One sent with enter doesn't.
func TestVerdictSaysItWentOnItsOwn(t *testing.T) {
	const went = "It went on its own at 09:00:04."
	tests := []struct {
		state engine.State
		note  string
		auto  bool
		want  string
	}{
		{engine.StateFixed, "", true, "✓ fixed, it stopped appearing after claude (tab 2) finished. " + went},
		{engine.StateFixed, "", false, "✓ fixed, it stopped appearing after claude (tab 2) finished"},
		{engine.StateUnverified, "", true, "? unverified, nothing ran to prove it either way. " + went + " Check it yourself"},
		{engine.StateUnverified, "", false, "? unverified, nothing ran to prove it either way. Check it yourself"},
		{engine.StateUnverified, "the pane went quiet", true, "? unverified, the pane went quiet. " + went + " Send it again or check it yourself"},
		{engine.StateUnverified, "the pane went quiet", false, "? unverified, the pane went quiet. Send it again or check it yourself"},
		{engine.StateStillFailing, "", true, "✗ still failing after claude (tab 2) finished, so queue mode stopped for this pane. " + went + " → claude (tab 2)"},
		{engine.StateStillFailing, "", false, "✗ still failing after claude (tab 2) finished. → claude (tab 2)"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%v %q auto %v", tt.state, tt.note, tt.auto), func(t *testing.T) {
			h := newHarness(t)
			en := h.catchError()
			en.State, en.Note, en.Agent = tt.state, tt.note, agentPane
			en.Auto, en.SentAt = tt.auto, time.Date(2026, 1, 1, 9, 0, 4, 0, time.UTC)
			h.model.labels[agentPane] = "claude (tab 2)" // as the send would have kept it
			if who, _ := h.model.entryTarget(en); who != tt.want {
				t.Errorf("line = %q\nwant   %q", who, tt.want)
			}
		})
	}
}
