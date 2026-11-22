package herdrtest_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/herdr"
	"github.com/swmcc/herdr-sightline/internal/herdr/herdrtest"
)

// failsafe stops a broken test hanging. Nothing here should come close to it.
const failsafe = 5 * time.Second

func dial(t *testing.T, h *herdrtest.Herdr) (context.Context, *herdr.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), failsafe)
	t.Cleanup(cancel)
	c, err := herdr.Dial(ctx, h.Path, herdr.WithBackoff(time.Millisecond, 10*time.Millisecond))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return ctx, c
}

// workspace is a tab with a dev server and an agent in it.
func workspace(t *testing.T) *herdrtest.Herdr {
	t.Helper()
	h := herdrtest.NewHerdr(t)
	h.AddTab(herdrtest.Tab{ID: "w1:t1", WorkspaceID: "w1", Number: 1})
	h.AddPane(herdrtest.Pane{ID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", Agent: "claude", AgentStatus: "idle", AgentName: "fixer"})
	h.AddPane(herdrtest.Pane{ID: "w1:p2", WorkspaceID: "w1", TabID: "w1:t1", Text: "listening on :3000"})
	return h
}

func TestHerdrLists(t *testing.T) {
	h := workspace(t)
	ctx, c := dial(t, h)

	if _, err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	panes, err := c.ListPanes(ctx)
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	want := []herdr.Pane{
		{ID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", Agent: "claude", AgentStatus: herdr.AgentIdle},
		{ID: "w1:p2", WorkspaceID: "w1", TabID: "w1:t1"},
	}
	if !slices.Equal(panes, want) {
		t.Errorf("ListPanes = %+v, want %+v", panes, want)
	}

	agents, err := c.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].PaneID != "w1:p1" || agents[0].Name != "fixer" || agents[0].StateChangeSeq == 0 {
		t.Errorf("ListAgents = %+v, want only the agent in w1:p1, named and stamped", agents)
	}

	tabs, err := c.ListTabs(ctx)
	if err != nil {
		t.Fatalf("ListTabs: %v", err)
	}
	if len(tabs) != 1 || tabs[0].ID != "w1:t1" {
		t.Errorf("ListTabs = %+v, want w1:t1", tabs)
	}
}

func TestHerdrReadsPanes(t *testing.T) {
	tests := []struct {
		name          string
		write         []string
		lines         int
		wantText      string
		wantTruncated bool
	}{
		{name: "all of it", lines: 10, wantText: "listening on :3000"},
		{name: "written lines follow", write: []string{"GET / 500", "TypeError: x is undefined"}, lines: 10,
			wantText: "listening on :3000\nGET / 500\nTypeError: x is undefined"},
		{name: "only the last lines", write: []string{"a", "b"}, lines: 2, wantText: "a\nb", wantTruncated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := workspace(t)
			ctx, c := dial(t, h)
			h.Write("w1:p2", tt.write...)

			read, err := c.ReadPane(ctx, "w1:p2", tt.lines)
			if err != nil {
				t.Fatalf("ReadPane: %v", err)
			}
			if read.Text != tt.wantText || read.Truncated != tt.wantTruncated || read.TabID != "w1:t1" {
				t.Errorf("ReadPane = %+v, want text %q and truncated %v", read, tt.wantText, tt.wantTruncated)
			}
		})
	}
}

func TestHerdrMissingPane(t *testing.T) {
	h := workspace(t)
	ctx, c := dial(t, h)

	if _, err := c.ReadPane(ctx, "w1:p9", 1); !herdr.IsPaneNotFound(err) {
		t.Errorf("ReadPane of a missing pane: err = %v, want pane not found", err)
	}
	if err := c.Prompt(ctx, "w1:p2", "hello"); !herdr.IsPaneNotFound(err) {
		t.Errorf("Prompt to a pane with no agent: err = %v, want pane not found", err)
	}
	if got := h.Prompts(); len(got) != 0 {
		t.Errorf("Prompts = %+v, want none", got)
	}
}

func TestHerdrRecordsPrompts(t *testing.T) {
	h := workspace(t)
	ctx, c := dial(t, h)

	for _, target := range []string{"w1:p1", "fixer"} {
		if err := c.Prompt(ctx, target, "fix it"); err != nil {
			t.Fatalf("Prompt(%s): %v", target, err)
		}
	}
	want := []herdrtest.Prompt{{Target: "w1:p1", Text: "fix it"}, {Target: "fixer", Text: "fix it"}}
	if got := h.Prompts(); !slices.Equal(got, want) {
		t.Errorf("Prompts = %+v, want %+v", got, want)
	}
}

func nextEvent(t *testing.T, events <-chan herdr.Event) herdr.Event {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("events closed, want another event")
		}
		return ev
	case <-time.After(failsafe):
		t.Fatal("no event arrived")
		return herdr.Event{}
	}
}

// TestHerdrEvents checks both envelope shapes reach the client, and only the
// subscriptions that asked for them.
func TestHerdrEvents(t *testing.T) {
	h := workspace(t)
	ctx, c := dial(t, h)

	status, err := c.Subscribe(ctx, herdr.AgentStatusChanged("w1:p1"))
	if err != nil {
		t.Fatalf("Subscribe to status: %v", err)
	}
	closed, err := c.Subscribe(ctx, herdr.PaneClosed())
	if err != nil {
		t.Fatalf("Subscribe to closing: %v", err)
	}

	h.SetAgentStatus("w1:p1", "working")
	ev := nextEvent(t, status)
	if ev.Kind != herdr.EventAgentStatusChanged || ev.Name != "pane.agent_status_changed" ||
		ev.PaneID != "w1:p1" || ev.AgentStatus != herdr.AgentWorking || ev.Agent != "claude" {
		t.Errorf("status event = %+v, want w1:p1 working, dotted", ev)
	}

	h.ClosePane("w1:p2")
	ev = nextEvent(t, closed)
	if ev.Kind != herdr.EventPaneClosed || ev.Name != "pane_closed" || ev.PaneID != "w1:p2" || ev.WorkspaceID != "w1" {
		t.Errorf("closed event = %+v, want w1:p2 closed, underscored", ev)
	}

	// Neither subscription heard the other's event. Another status change
	// arriving next on the status stream proves nothing else was queued there.
	h.SetAgentStatus("w1:p1", "done")
	if ev := nextEvent(t, status); ev.AgentStatus != herdr.AgentDone {
		t.Errorf("status event = %+v, want done next", ev)
	}
	select {
	case ev := <-closed:
		t.Errorf("closing stream heard %+v, want nothing", ev)
	default:
	}
}

func TestHerdrRestart(t *testing.T) {
	h := workspace(t)
	ctx, c := dial(t, h)

	events, err := c.Subscribe(ctx, herdr.AgentStatusChanged("w1:p1"))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	h.Restart()
	if ev := nextEvent(t, events); ev.Kind != herdr.EventReconnected {
		t.Fatalf("after a restart got %+v, want reconnected", ev)
	}
	h.SetAgentStatus("w1:p1", "blocked")
	if ev := nextEvent(t, events); ev.AgentStatus != herdr.AgentBlocked {
		t.Errorf("after reconnecting got %+v, want blocked", ev)
	}
}
