package herdr_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/herdr"
	"github.com/swmcc/herdr-sightline/internal/herdr/herdrtest"
)

// t0 is the fake clock's start. Tests move time on by passing later times, so
// nothing ever sleeps.
var t0 = time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)

func at(seconds int) time.Time { return t0.Add(time.Duration(seconds) * time.Second) }

// loadSnapshot serves a fixture's pane_list, agent_list and tab_list from a
// fake herdr and reads them back through the client, so the fixtures also
// check that herdr's real shapes decode.
func loadSnapshot(t *testing.T, name string) herdr.AgentSnapshot {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "agents", name))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Panes  json.RawMessage `json:"pane_list"`
		Agents json.RawMessage `json:"agent_list"`
		Tabs   json.RawMessage `json:"tab_list"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decoding %s: %v", name, err)
	}
	srv := herdrtest.New(t)
	srv.Handle("pane.list", reply(fixture.Panes))
	srv.Handle("agent.list", reply(fixture.Agents))
	srv.Handle("tab.list", reply(fixture.Tabs))
	c := newClient(t, srv)
	ctx := context.Background()

	var snap herdr.AgentSnapshot
	if snap.Panes, err = c.ListPanes(ctx); err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if snap.Agents, err = c.ListAgents(ctx); err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if snap.Tabs, err = c.ListTabs(ctx); err != nil {
		t.Fatalf("ListTabs: %v", err)
	}
	return snap
}

// withStatus returns a copy of snap where the given agents have a new status
// and the dropped ones have left their panes, as a later pane.list and
// agent.list would report.
func withStatus(snap herdr.AgentSnapshot, statuses map[string]herdr.AgentStatus, drop ...string) herdr.AgentSnapshot {
	out := herdr.AgentSnapshot{Tabs: snap.Tabs}
	for _, p := range snap.Panes {
		if s, ok := statuses[p.ID]; ok {
			p.AgentStatus = s
		}
		if slices.Contains(drop, p.ID) {
			p.Agent, p.DisplayAgent, p.AgentStatus = "", "", herdr.AgentUnknown
		}
		out.Panes = append(out.Panes, p)
	}
	for _, a := range snap.Agents {
		if s, ok := statuses[a.PaneID]; ok {
			a.Status = s
		}
		if !slices.Contains(drop, a.PaneID) {
			out.Agents = append(out.Agents, a)
		}
	}
	return out
}

// step is one thing that happens to the tracker: an event, or a fresh
// snapshot with some statuses changed and some agents gone. A snapshot with
// taken set was asked for at that second, so events since then are newer.
type step struct {
	at    int
	ev    *herdr.Event
	sync  map[string]herdr.AgentStatus
	drop  []string
	taken *int
}

func sec(n int) *int { return &n }

func status(pane string, s herdr.AgentStatus) *herdr.Event {
	agent := "claude"
	if pane == "w1:p3" {
		agent = "codex"
	}
	return &herdr.Event{Kind: herdr.EventAgentStatusChanged, PaneID: pane, WorkspaceID: "w1", AgentStatus: s, Agent: agent}
}

func exited(pane string) *herdr.Event {
	return &herdr.Event{Kind: herdr.EventPaneExited, PaneID: pane, WorkspaceID: "w1"}
}

func left(pane string) *herdr.Event {
	return &herdr.Event{Kind: herdr.EventAgentStatusChanged, PaneID: pane, WorkspaceID: "w1", AgentStatus: herdr.AgentUnknown}
}

func closed(pane string) *herdr.Event {
	return &herdr.Event{Kind: herdr.EventPaneClosed, PaneID: pane, WorkspaceID: "w1"}
}

func play(t *testing.T, tracker *herdr.AgentTracker, snap herdr.AgentSnapshot, steps []step) {
	t.Helper()
	for _, s := range steps {
		switch {
		case s.ev != nil:
			tracker.Observe(*s.ev, at(s.at))
		case s.sync != nil:
			fresh := withStatus(snap, s.sync, s.drop...)
			if s.taken != nil {
				fresh.Taken = at(*s.taken)
			}
			tracker.Sync(fresh, at(s.at))
		}
	}
}

func TestChoose(t *testing.T) {
	snap := loadSnapshot(t, "two-workspaces.json")

	tests := []struct {
		name string
		// initial changes statuses in the first snapshot the tracker sees.
		initial map[string]herdr.AgentStatus
		steps   []step
		watched string
		pins    []config.Pin
		// want is the pane ID chosen, or empty when it should ask.
		want      string
		rule      herdr.ChoiceRule
		ask       herdr.AskReason
		unmatched *config.Pin
	}{
		{
			name:    "before any activity the latest state change wins",
			watched: "w1:p1",
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name: "the agent that started work last wins",
			steps: []step{
				{at: 1, ev: status("w1:p2", herdr.AgentWorking)},
				{at: 2, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 3, ev: status("w1:p3", herdr.AgentIdle)},
				{at: 4, ev: status("w1:p2", herdr.AgentIdle)},
			},
			watched: "w1:p1",
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name: "starting work again takes it back",
			steps: []step{
				{at: 1, ev: status("w1:p2", herdr.AgentWorking)},
				{at: 2, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 3, ev: status("w1:p2", herdr.AgentIdle)},
				{at: 4, ev: status("w1:p2", herdr.AgentWorking)},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name: "finishing, blocking and repeats of working aren't activity",
			steps: []step{
				{at: 1, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 2, ev: status("w1:p2", herdr.AgentWorking)},
				{at: 3, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 4, ev: status("w1:p3", herdr.AgentBlocked)},
				{at: 5, ev: status("w1:p3", herdr.AgentDone)},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "an agent already working when first seen is active then",
			initial: map[string]herdr.AgentStatus{"w1:p2": herdr.AgentWorking},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "a snapshot showing an agent has started work counts",
			steps:   []step{{at: 1, sync: map[string]herdr.AgentStatus{"w1:p2": herdr.AgentWorking}}},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "an agent missing from a fresh snapshot is forgotten",
			steps:   []step{{at: 1, sync: map[string]herdr.AgentStatus{}, drop: []string{"w1:p3"}}},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "a stale snapshot doesn't undo newer events",
			initial: map[string]herdr.AgentStatus{"w1:p2": herdr.AgentWorking},
			steps: []step{
				{at: 2, ev: status("w1:p2", herdr.AgentIdle)},
				{at: 3, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 4, sync: map[string]herdr.AgentStatus{"w1:p2": herdr.AgentWorking}, taken: sec(1)},
			},
			watched: "w1:p1",
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "a real start after a stale snapshot still counts",
			initial: map[string]herdr.AgentStatus{"w1:p2": herdr.AgentWorking},
			steps: []step{
				{at: 2, ev: status("w1:p2", herdr.AgentIdle)},
				{at: 3, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 4, sync: map[string]herdr.AgentStatus{"w1:p2": herdr.AgentWorking}, taken: sec(1)},
				{at: 5, ev: status("w1:p2", herdr.AgentWorking)},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name: "a stale snapshot doesn't bring back an agent that left",
			steps: []step{
				{at: 2, ev: left("w1:p3")},
				{at: 3, sync: map[string]herdr.AgentStatus{}, taken: sec(1)},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name: "a stale snapshot doesn't drop an agent heard about since",
			steps: []step{
				{at: 1, sync: map[string]herdr.AgentStatus{}, drop: []string{"w1:p3"}},
				{at: 3, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 4, sync: map[string]herdr.AgentStatus{}, drop: []string{"w1:p3"}, taken: sec(2)},
			},
			watched: "w1:p1",
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name: "a snapshot with no time taken is trusted over events",
			steps: []step{
				{at: 2, ev: left("w1:p3")},
				{at: 3, sync: map[string]herdr.AgentStatus{}},
			},
			watched: "w1:p1",
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "a pane whose process exited loses its agent",
			steps:   []step{{at: 1, ev: exited("w1:p3")}},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name: "a new agent in a pane doesn't inherit the old one's kind",
			steps: []step{{at: 1, ev: &herdr.Event{
				Kind: herdr.EventAgentStatusChanged, PaneID: "w1:p3", WorkspaceID: "w1",
				AgentStatus: herdr.AgentIdle, Agent: "claude",
			}}},
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "Codex"}},
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
			unmatched: &config.Pin{PaneTitle: "web", Agent: "Codex"},
		},
		{
			name: "activity survives a fresh snapshot",
			steps: []step{
				{at: 1, ev: status("w1:p2", herdr.AgentWorking)},
				{at: 2, sync: map[string]herdr.AgentStatus{}},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "a pin by kind beats activity",
			steps:   []step{{at: 1, ev: status("w1:p2", herdr.AgentWorking)}},
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "codex"}},
			want:    "w1:p3", rule: herdr.ChosenByPin,
		},
		{
			name:    "a pin matches the kind herdr shows, ignoring case",
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "CODEX"}},
			want:    "w1:p3", rule: herdr.ChosenByPin,
		},
		{
			name:    "a pin by herdr name can reach another tab",
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "reviewer"}},
			want:    "w1:p4", rule: herdr.ChosenByPin,
		},
		{
			name:    "a pin by pane label can reach another workspace",
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "claude-docs"}},
			want:    "w2:p1", rule: herdr.ChosenByPin,
		},
		{
			name:    "a pin's pane title can be the terminal title",
			watched: "w2:p2",
			pins:    []config.Pin{{PaneTitle: "hugo server", Agent: "codex"}},
			want:    "w1:p3", rule: herdr.ChosenByPin,
		},
		{
			name:    "a pin by kind with several matches prefers the same tab",
			watched: "w1:p5",
			pins:    []config.Pin{{PaneTitle: "api", Agent: "claude"}},
			want:    "w1:p4", rule: herdr.ChosenByPin,
		},
		{
			name:    "a pin by kind with several matches in other tabs takes the most recent",
			steps:   []step{{at: 1, ev: status("w2:p1", herdr.AgentWorking)}},
			watched: "w1:p6",
			pins:    []config.Pin{{PaneTitle: "swm@gollum:~/src/worker", Agent: "claude"}},
			want:    "w2:p1", rule: herdr.ChosenByPin,
		},
		{
			name:    "the first pin that names the pane decides",
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "reviewer"}, {PaneTitle: "web", Agent: "claude"}},
			want:    "w1:p4", rule: herdr.ChosenByPin,
		},
		{
			name:    "a pin for an agent that isn't there falls back to the tab and says so",
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "gemini"}, {PaneTitle: "web", Agent: "reviewer"}},
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
			unmatched: &config.Pin{PaneTitle: "web", Agent: "gemini"},
		},
		{
			name:      "a pin for an agent that isn't there still asks when the tab is empty",
			watched:   "w1:p6",
			pins:      []config.Pin{{PaneTitle: "swm@gollum:~/src/worker", Agent: "gemini"}},
			ask:       herdr.AskNoAgentInTab,
			unmatched: &config.Pin{PaneTitle: "swm@gollum:~/src/worker", Agent: "gemini"},
		},
		{
			name:    "a pin for another pane is ignored",
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "api", Agent: "reviewer"}},
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "no agent in the tab asks rather than reaching into another",
			watched: "w1:p6",
			ask:     herdr.AskNoAgentInTab,
		},
		{
			name:    "a pane herdr doesn't know asks",
			watched: "w9:p9",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "codex"}},
			ask:     herdr.AskUnknownPane,
		},
		{
			name:    "the watched pane isn't chosen for itself",
			steps:   []step{{at: 1, ev: status("w1:p2", herdr.AgentWorking)}},
			watched: "w1:p2",
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "an agent that left its pane isn't chosen",
			steps:   []step{{at: 1, ev: left("w1:p3")}},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "a closed pane is forgotten",
			steps:   []step{{at: 1, ev: closed("w1:p3")}},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "once every agent in the tab has gone it asks",
			steps:   []step{{at: 1, ev: closed("w1:p3")}, {at: 2, ev: left("w1:p2")}},
			watched: "w1:p1",
			ask:     herdr.AskNoAgentInTab,
		},
		{
			name:    "an agent in the watched pane itself doesn't fill its tab",
			steps:   []step{{at: 1, ev: status("w1:p6", herdr.AgentWorking)}},
			watched: "w1:p6",
			ask:     herdr.AskNoAgentInTab,
		},
		{
			name:    "a status event for a pane with no agent changes nothing",
			steps:   []step{{at: 1, ev: left("w1:p5")}},
			watched: "w1:p5",
			want:    "w1:p4", rule: herdr.ChosenBySameTab,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := herdr.NewAgentTracker()
			tracker.Sync(withStatus(snap, tt.initial), t0)
			play(t, tracker, snap, tt.steps)

			got := tracker.Choose(tt.watched, tt.pins)
			if got.Chosen() != (tt.want != "") {
				t.Fatalf("Choose = %+v, want chosen %v", got, tt.want != "")
			}
			if got.Target.PaneID != tt.want || got.Rule != tt.rule || got.Ask != tt.ask {
				t.Errorf("Choose = %s by %q, ask %q; want %s by %q, ask %q",
					got.Target.PaneID, got.Rule, got.Ask, tt.want, tt.rule, tt.ask)
			}
			if !reflect.DeepEqual(got.UnmatchedPin, tt.unmatched) {
				t.Errorf("UnmatchedPin = %+v, want %+v", got.UnmatchedPin, tt.unmatched)
			}
		})
	}
}

func TestChooseTarget(t *testing.T) {
	snap := loadSnapshot(t, "two-workspaces.json")
	tracker := herdr.NewAgentTracker()
	tracker.Sync(snap, t0)

	got := tracker.Choose("w1:p1", []config.Pin{{PaneTitle: "web", Agent: "reviewer"}})
	want := herdr.Target{
		PaneID: "w1:p4", WorkspaceID: "w1", TabID: "w1:t2", TabNumber: 2,
		Kind: "claude", Name: "reviewer", Status: herdr.AgentIdle, Label: "reviewer (tab 2)",
	}
	if got.Target != want {
		t.Errorf("Target =\n%+v\nwant\n%+v", got.Target, want)
	}
}

func TestCandidates(t *testing.T) {
	snap := loadSnapshot(t, "two-workspaces.json")

	tests := []struct {
		name    string
		steps   []step
		watched string
		want    []string
	}{
		{
			name:    "same tab first, then by activity",
			steps:   []step{{at: 1, ev: status("w1:p2", herdr.AgentWorking)}},
			watched: "w1:p1",
			want:    []string{"claude (tab 1)", "Codex (tab 1)", "reviewer (tab 2)", "claude-docs (workspace w2, tab 1)"},
		},
		{
			name:    "a tab with no agents lists every agent",
			steps:   []step{{at: 1, ev: status("w2:p1", herdr.AgentWorking)}},
			watched: "w1:p6",
			want:    []string{"claude-docs (workspace w2, tab 1)", "reviewer (tab 2)", "Codex (tab 1)", "claude (tab 1)"},
		},
		{
			name:    "the watched pane is left out",
			watched: "w1:p4",
			want:    []string{"Codex (tab 1)", "claude (tab 1)", "claude-docs (workspace w2, tab 1)"},
		},
		{
			name:    "a new agent heard about by event is listed with what's known",
			steps:   []step{{at: 1, ev: status("w1:p6", herdr.AgentWorking)}},
			watched: "w1:p1",
			want:    []string{"Codex (tab 1)", "claude (tab 1)", "claude (tab 3)", "reviewer (tab 2)", "claude-docs (workspace w2, tab 1)"},
		},
		{
			name:    "an unknown pane gets every agent by activity",
			watched: "w9:p9",
			want: []string{
				"reviewer (workspace w1, tab 2)", "Codex (workspace w1, tab 1)",
				"claude (workspace w1, tab 1)", "claude-docs (workspace w2, tab 1)",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := herdr.NewAgentTracker()
			tracker.Sync(snap, t0)
			play(t, tracker, snap, tt.steps)

			var got []string
			for _, c := range tracker.Candidates(tt.watched) {
				got = append(got, c.Label)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Candidates =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

// Without agent.list there are no names or state change counters, but agents
// in pane.list still count.
func TestSyncFromPanesAlone(t *testing.T) {
	snap := loadSnapshot(t, "two-workspaces.json")
	snap.Agents = nil
	tracker := herdr.NewAgentTracker()
	tracker.Sync(snap, t0)

	var got []string
	for _, c := range tracker.Candidates("w1:p1") {
		got = append(got, c.PaneID+" "+c.Name)
	}
	want := []string{"w1:p2 claude", "w1:p3 Codex", "w1:p4 claude", "w2:p1 claude-docs"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Candidates = %q, want %q", got, want)
	}
}

func TestListAgents(t *testing.T) {
	snap := loadSnapshot(t, "two-workspaces.json")
	want := herdr.AgentInfo{
		PaneID: "w1:p4", WorkspaceID: "w1", TabID: "w1:t2", Agent: "claude",
		Name: "reviewer", Status: herdr.AgentIdle, StateChangeSeq: 230,
	}
	if len(snap.Agents) != 4 || snap.Agents[2] != want {
		t.Errorf("ListAgents = %+v, want 4 agents with %+v third", snap.Agents, want)
	}
	if tab := snap.Tabs[0]; tab != (herdr.Tab{ID: "w1:t1", WorkspaceID: "w1", Number: 1, Label: "Agentic"}) {
		t.Errorf("ListTabs()[0] = %+v", tab)
	}
}

// The feed, the event reader and a periodic resync may all touch the tracker
// at once. This is for -race to check.
func TestTrackerConcurrentUse(t *testing.T) {
	snap := loadSnapshot(t, "two-workspaces.json")
	tracker := herdr.NewAgentTracker()
	tracker.Sync(snap, t0)

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				now := at(i*100 + j)
				switch i {
				case 0:
					tracker.Observe(*status("w1:p2", herdr.AgentWorking), now)
				case 1:
					tracker.Sync(snap, now)
				case 2:
					tracker.Choose("w1:p1", nil)
				case 3:
					tracker.Candidates("w1:p1")
				}
			}
		}()
	}
	wg.Wait()
}
