package feed

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/herdr"
	"github.com/swmcc/herdr-sightline/internal/herdr/herdrtest"
)

// These tests play the attacker from the injection review: someone who
// controls what a watched pane prints, including its terminal title.

// titleFlip sets up a pin on a terminal title to a named agent in another tab,
// with the watched pane unlabelled and titled something else. Setting title to
// the pinned one moves the target to the other tab.
func titleFlip(t *testing.T) (*harness, func(string)) {
	t.Helper()
	h := newHarness(t)
	var mu sync.Mutex
	title := "next dev"
	h.herdr.srv.Handle("pane.list", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		mu.Lock()
		defer mu.Unlock()
		return map[string]any{"type": "pane_list", "panes": []map[string]any{
			{"pane_id": webPane, "workspace_id": "w1", "tab_id": "w1:t2", "terminal_title_stripped": title},
			{"pane_id": feedPane, "workspace_id": "w1", "tab_id": "w1:t2"},
			{"pane_id": agentPane, "workspace_id": "w1", "tab_id": "w1:t2", "agent": "claude", "agent_status": "idle"},
			{"pane_id": otherAgent, "workspace_id": "w1", "tab_id": "w1:t3", "agent": "claude", "agent_status": "idle"},
		}}, nil
	})
	h.herdr.srv.Handle("agent.list", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		return map[string]any{"type": "agent_list", "agents": []map[string]any{
			{"pane_id": agentPane, "workspace_id": "w1", "tab_id": "w1:t2", "agent": "claude", "agent_status": "idle", "state_change_seq": 1},
			{"pane_id": otherAgent, "workspace_id": "w1", "tab_id": "w1:t3", "agent": "claude", "name": "prod-ops", "agent_status": "idle", "state_change_seq": 1},
		}}, nil
	})
	cfg := config.Default()
	cfg.Pins = []config.Pin{{PaneTitle: "billing worker", Agent: "prod-ops"}}
	h.model.applyConfig(config.Update{Config: cfg})
	return h, func(s string) {
		mu.Lock()
		title = s
		mu.Unlock()
		h.sync()
	}
}

func TestTitleFlipAfterViewIsRefused(t *testing.T) {
	h, setTitle := titleFlip(t)
	h.catchError()
	h.press("v")
	if !strings.Contains(h.model.View(), "to claude (tab 2)") {
		t.Fatalf("view header doesn't name the agent in this tab:\n%s", h.model.View())
	}

	setTitle("billing worker")
	h.run(h.press("enter"))
	if calls := h.herdr.promptCalls(); len(calls) != 0 {
		t.Fatalf("enter sent %+v after the target changed under the view", calls)
	}
	if !strings.Contains(h.model.status, "changed") {
		t.Errorf("status = %q, want it to say the agent changed", h.model.status)
	}
	if !strings.Contains(h.model.View(), "prod-ops") {
		t.Errorf("the view wasn't redrawn with the new agent:\n%s", h.model.View())
	}

	// Even now the view names it, another tab needs a second enter.
	h.run(h.press("enter"))
	if calls := h.herdr.promptCalls(); len(calls) != 0 {
		t.Fatalf("one enter sent to an agent in another tab: %+v", calls)
	}
	if !strings.Contains(h.model.status, "another tab") {
		t.Errorf("status = %q, want it to say the agent is in another tab", h.model.status)
	}
}

func TestPinToAnotherTabNeedsASecondEnter(t *testing.T) {
	h, setTitle := titleFlip(t)
	h.catchError()
	setTitle("billing worker")
	h.run(h.press("enter"))
	if calls := h.herdr.promptCalls(); len(calls) != 0 {
		t.Fatalf("one enter sent to an agent in another tab: %+v", calls)
	}
	h.run(h.press("enter"))
	calls := h.herdr.promptCalls()
	if len(calls) != 1 || calls[0].Target != otherAgent {
		t.Errorf("prompts = %+v, want one to %s after confirming", calls, otherAgent)
	}
}

func TestViewDrawsTabsAsATerminalWould(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.update(watchListMsg{panes: []string{webPane}})
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: []string{"TypeError: a\tb", `TypeError: a\x09b`}}})
	h.clock.Advance(2 * time.Second)
	h.update(tickMsg{})
	if len(h.model.entries) == 0 {
		t.Fatal("no entry")
	}
	h.press("v")
	if !strings.Contains(h.model.viewText, "a\tb") {
		t.Fatalf("the message lost its tab: %q", h.model.viewText)
	}
	drawn := strings.Join(h.model.viewLines(), "\n")
	if strings.Count(drawn, `\x09`) != strings.Count(h.model.viewText, `\x09`) {
		t.Errorf("a tab is drawn like the four characters a pane can print:\n%s", drawn)
	}
}

func TestCleanShowsWhatATerminalWouldAct(t *testing.T) {
	tests := []struct{ in, want string }{
		{"web", "web"},
		{"web\x1b[2J\x07", `web\x1b[2J\x07`},
		{"web\u009b2J", `web\x9b2J`},
		{"web\x9b2J", `web\x9b2J`}, // not valid UTF-8
		{"ok ✓", "ok ✓"},
	}
	for _, tt := range tests {
		if got := clean(tt.in); got != tt.want {
			t.Errorf("clean(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSendThatMayHaveArrivedAsksBeforeAgain(t *testing.T) {
	tests := []struct {
		name      string
		reply     herdrtest.Handler
		wantMaybe bool
	}{
		{
			name: "reply lost after the request was written",
			reply: func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
				return herdrtest.Raw("not json\n"), nil
			},
			wantMaybe: true,
		},
		{
			name: "herdr refused it",
			reply: func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
				return nil, &herdrtest.ErrorBody{Code: "agent_blocked", Message: "agent is blocked"}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			var mu sync.Mutex
			calls := 0
			h.herdr.srv.Handle("agent.prompt", func(req herdrtest.Request) (any, *herdrtest.ErrorBody) {
				mu.Lock()
				calls++
				mu.Unlock()
				return tt.reply(req)
			})
			h.catchError()
			h.run(h.press("enter"))
			id := h.model.entries[0].ID
			if got := h.model.maybeSent[id] != ""; got != tt.wantMaybe {
				t.Errorf("maybe sent = %v, want %v (status %q)", got, tt.wantMaybe, h.model.status)
			}

			cmd := h.press("enter")
			if tt.wantMaybe && cmd != nil {
				t.Error("enter sent again straight after a send that may have arrived")
			}
			if !tt.wantMaybe && cmd == nil {
				t.Errorf("enter after a refusal didn't try again, status %q", h.model.status)
			}
			h.run(cmd)
			mu.Lock()
			defer mu.Unlock()
			want := 1
			if !tt.wantMaybe {
				want = 2
			}
			if calls != want {
				t.Errorf("got %d prompts, want %d", calls, want)
			}
		})
	}
}

func TestMessagesFromAStoppedLoopAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.update(watchListMsg{panes: []string{webPane}})
	old := h.gen(webPane)
	h.update(watchListMsg{panes: nil})
	h.update(watchListMsg{panes: []string{webPane}})
	h.update(paneGoneMsg{pane: webPane, gen: old})
	if !h.model.isWatching(webPane) {
		t.Error("a late message from the old loop dropped the pane watched again")
	}
}

func TestMoveAfterTheOldIDLooksClosed(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.update(watchListMsg{panes: []string{webPane}})
	// The read of the old id wins the race with pane.moved.
	h.update(paneGoneMsg{pane: webPane, gen: h.gen(webPane)})
	moved := &herdr.Pane{ID: "w2:p1", WorkspaceID: "w2", TabID: "w2:t1"}
	h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneMoved, PaneID: "w2:p1", PreviousPaneID: webPane, Pane: moved}, global: true})
	if got := h.model.watching; len(got) != 1 || got[0] != "w2:p1" {
		t.Errorf("watching %v, want the moved pane", got)
	}
}

func TestSnapshotFromBeforeTheSendDoesntFinishIt(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	// A snapshot asked for while the agent was still working on something
	// else, before the send, arrives after it.
	h.herdr.setStatus(agentPane, herdr.AgentWorking)
	stale := h.loops.snapshot()
	h.clock.Advance(time.Second)
	h.run(h.press("enter"))
	h.update(stale)
	h.herdr.setStatus(agentPane, herdr.AgentIdle)
	h.herdr.mu.Lock()
	h.herdr.seq[agentPane] = 0 // the counter alone mustn't decide it either
	h.herdr.mu.Unlock()
	h.sync()
	if _, ok := h.model.awaiting[agentPane]; !ok {
		t.Error("a snapshot from before the send counted as the agent working on it")
	}
}

func TestConfirmIsForOneAgent(t *testing.T) {
	h, setTitle := titleFlip(t)
	h.catchError()
	h.run(h.press("enter"))
	if calls := h.herdr.promptCalls(); len(calls) != 1 || calls[0].Target != agentPane {
		t.Fatalf("first send = %+v", calls)
	}
	// Enter once to send it again, then the pane retitles itself to match a
	// pin to an agent in another tab before the second enter.
	h.press("enter")
	setTitle("billing worker")
	h.run(h.press("enter"))
	if calls := h.herdr.promptCalls(); len(calls) != 1 {
		t.Fatalf("the confirm given for %s sent to %s: status %q", agentPane, calls[len(calls)-1].Target, h.model.status)
	}
	if !strings.Contains(h.model.status, "prod-ops") {
		t.Errorf("status = %q, want it to name the new agent", h.model.status)
	}
}

func TestFloodDoesntMoveTheCursorToTheNewest(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.update(watchListMsg{panes: []string{webPane}})
	feed := func(i int) {
		line := "panic: failure " + string(rune('a'+i%26)) + string(rune('a'+i/26))
		h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: []string{line}}})
		h.clock.Advance(2 * time.Second)
		h.update(tickMsg{})
	}
	for i := range 20 {
		feed(i)
	}
	// Select the oldest, then flood until it's dropped.
	h.model.selected = h.model.entries[len(h.model.entries)-1].ID
	for i := 20; i < 400 && h.model.engine.Dropped() == 0; i++ {
		feed(i)
	}
	if h.model.engine.Dropped() == 0 {
		t.Fatal("the flood dropped nothing")
	}
	if h.model.selected == h.model.entries[0].ID {
		t.Error("dropping the selected entry put the newest, the attacker's, under the cursor")
	}
}
