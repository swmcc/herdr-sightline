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

// change is how a later pane.list and agent.list differ from the fixture:
// some agents have a new status or state change counter and some have left
// their panes.
type change struct {
	status map[string]herdr.AgentStatus
	seq    map[string]uint64
	drop   []string
}

func (c change) apply(snap herdr.AgentSnapshot) herdr.AgentSnapshot {
	out := herdr.AgentSnapshot{Tabs: snap.Tabs}
	for _, p := range snap.Panes {
		if s, ok := c.status[p.ID]; ok {
			p.AgentStatus = s
		}
		if slices.Contains(c.drop, p.ID) {
			p.Agent, p.DisplayAgent, p.AgentStatus = "", "", herdr.AgentUnknown
		}
		out.Panes = append(out.Panes, p)
	}
	for _, a := range snap.Agents {
		if s, ok := c.status[a.PaneID]; ok {
			a.Status = s
		}
		if n, ok := c.seq[a.PaneID]; ok {
			a.StateChangeSeq = n
		}
		if !slices.Contains(c.drop, a.PaneID) {
			out.Agents = append(out.Agents, a)
		}
	}
	return out
}

// step is one thing that happens to the tracker: an event, or a fresh
// snapshot with a change from the fixture. A snapshot with taken set was
// asked for at that second, so events since then are newer.
type step struct {
	at    int
	ev    *herdr.Event
	sync  *change
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

// movedTo says w1:p3, the codex pane, moved to a new ID in the same tab.
func movedTo(id string) *herdr.Event {
	return &herdr.Event{
		Kind: herdr.EventPaneMoved, PaneID: id, WorkspaceID: "w1", PreviousPaneID: "w1:p3",
		Pane: &herdr.Pane{ID: id, WorkspaceID: "w1", TabID: "w1:t1", Agent: "codex", AgentStatus: herdr.AgentWorking},
	}
}

func working(panes ...string) map[string]herdr.AgentStatus {
	out := map[string]herdr.AgentStatus{}
	for _, p := range panes {
		out[p] = herdr.AgentWorking
	}
	return out
}

func play(t *testing.T, tracker *herdr.AgentTracker, snap herdr.AgentSnapshot, steps []step) {
	t.Helper()
	for _, s := range steps {
		switch {
		case s.ev != nil:
			tracker.Observe(*s.ev, at(s.at))
		case s.sync != nil:
			fresh := s.sync.apply(snap)
			if s.taken != nil {
				fresh.Taken = at(*s.taken)
			}
			tracker.Sync(fresh, at(s.at))
		}
	}
}

// In the fixture, tab w1:t1 holds the watched pane "web" (w1:p1), an idle
// claude (w1:p2, counter 221) and an idle codex (w1:p3, counter 225). Tab
// w1:t2 holds a claude named reviewer (w1:p4) and a pane labelled "api". Tab
// w1:t3 holds only an unlabelled shell. Workspace w2 has a blocked claude in
// a pane labelled claude-docs (w2:p1) beside an unlabelled "hugo server".
func TestChoose(t *testing.T) {
	snap := loadSnapshot(t, "two-workspaces.json")

	tests := []struct {
		name string
		// initial changes the first snapshot the tracker sees.
		initial change
		steps   []step
		watched string
		pins    []config.Pin
		// want is the pane ID chosen, or empty when it should ask.
		want      string
		rule      herdr.ChoiceRule
		ask       herdr.AskReason
		pinField  herdr.PinField
		unmatched *config.Pin
	}{
		// Activity learned from events.
		{
			name:    "two agents nobody has seen start work is a tie",
			watched: "w1:p1",
			ask:     herdr.AskTie,
		},
		{
			name:    "two agents already working in the first snapshot is a tie",
			initial: change{status: working("w1:p2", "w1:p3")},
			watched: "w1:p1",
			ask:     herdr.AskTie,
		},
		{
			name:    "the only agent in the tab is picked without any activity",
			watched: "w1:p5",
			want:    "w1:p4", rule: herdr.ChosenBySameTab,
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
				{at: 3, ev: status("w1:p2", herdr.AgentDone)},
				{at: 4, ev: status("w1:p2", herdr.AgentWorking)},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name: "finishing and repeats of working aren't activity",
			steps: []step{
				{at: 1, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 2, ev: status("w1:p2", herdr.AgentWorking)},
				{at: 3, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 4, ev: status("w1:p3", herdr.AgentDone)},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name: "blocked to working is approving a prompt, not new work",
			steps: []step{
				{at: 1, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 2, ev: status("w1:p2", herdr.AgentWorking)},
				{at: 3, ev: status("w1:p3", herdr.AgentBlocked)},
				{at: 4, ev: status("w1:p3", herdr.AgentWorking)},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "an agent already working when first seen is active then",
			initial: change{status: working("w1:p2")},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},

		// Activity learned from snapshots alone, because herdr 0.9.1 may not
		// send status events at all.
		{
			name:    "a poll that sees an agent go from idle to working counts",
			steps:   []step{{at: 5, sync: &change{status: working("w1:p2")}}},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name: "a poll that sees an agent go from done to working counts",
			steps: []step{
				{at: 2, sync: &change{status: map[string]herdr.AgentStatus{"w1:p2": herdr.AgentWorking, "w1:p3": herdr.AgentDone}}},
				{at: 4, sync: &change{status: working("w1:p2", "w1:p3")}},
			},
			watched: "w1:p1",
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "a poll that sees the counter move on a working agent counts",
			initial: change{status: working("w1:p2", "w1:p3")},
			steps: []step{
				{at: 5, sync: &change{status: working("w1:p2", "w1:p3"), seq: map[string]uint64{"w1:p3": 300}}},
			},
			watched: "w1:p1",
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "a poll that sees blocked to working doesn't count",
			initial: change{status: working("w1:p2")},
			steps: []step{
				{at: 2, sync: &change{status: map[string]herdr.AgentStatus{"w1:p2": herdr.AgentWorking, "w1:p3": herdr.AgentBlocked}}},
				{at: 3, sync: &change{status: working("w1:p2", "w1:p3"), seq: map[string]uint64{"w1:p3": 300}}},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "the counter moving on an idle agent doesn't count",
			steps:   []step{{at: 2, sync: &change{seq: map[string]uint64{"w1:p3": 300}}}},
			watched: "w1:p1",
			ask:     herdr.AskTie,
		},
		{
			name: "an event and the next poll don't count the same start twice",
			steps: []step{
				{at: 1, ev: status("w1:p2", herdr.AgentWorking)},
				{at: 2, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 3, sync: &change{status: working("w1:p2", "w1:p3"), seq: map[string]uint64{"w1:p2": 300, "w1:p3": 301}}},
			},
			watched: "w1:p1",
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "an agent missing from a fresh snapshot is forgotten",
			steps:   []step{{at: 1, sync: &change{drop: []string{"w1:p3"}}}},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},

		// Snapshots that events have overtaken.
		{
			name:    "a stale snapshot doesn't undo newer events",
			initial: change{status: working("w1:p2")},
			steps: []step{
				{at: 2, ev: status("w1:p2", herdr.AgentIdle)},
				{at: 3, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 4, sync: &change{status: working("w1:p2")}, taken: sec(1)},
			},
			watched: "w1:p1",
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "a real start after a stale snapshot still counts",
			initial: change{status: working("w1:p2")},
			steps: []step{
				{at: 2, ev: status("w1:p2", herdr.AgentIdle)},
				{at: 3, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 4, sync: &change{status: working("w1:p2")}, taken: sec(1)},
				{at: 5, ev: status("w1:p2", herdr.AgentWorking)},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "a stale snapshot doesn't bring back an agent that left",
			initial: change{status: working("w1:p3")},
			steps: []step{
				{at: 2, ev: left("w1:p3")},
				{at: 3, sync: &change{status: working("w1:p3")}, taken: sec(1)},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "a snapshot with no time taken is trusted over events",
			initial: change{status: working("w1:p3")},
			steps: []step{
				{at: 2, ev: left("w1:p3")},
				{at: 3, sync: &change{status: working("w1:p3")}},
			},
			watched: "w1:p1",
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name: "a stale snapshot doesn't bring back an agent whose pane exited",
			steps: []step{
				{at: 1, sync: &change{drop: []string{"w1:p3"}}},
				{at: 2, ev: exited("w1:p3")},
				{at: 3, sync: &change{}, taken: sec(1)},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name: "a stale snapshot doesn't bring back a closed pane",
			steps: []step{
				{at: 2, ev: closed("w1:p1")},
				{at: 3, sync: &change{}, taken: sec(1)},
			},
			watched: "w1:p1",
			ask:     herdr.AskUnknownPane,
		},
		{
			name: "a stale snapshot doesn't drop an agent heard about since",
			steps: []step{
				{at: 1, sync: &change{drop: []string{"w1:p3"}}},
				{at: 3, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 4, sync: &change{drop: []string{"w1:p3"}}, taken: sec(2)},
			},
			watched: "w1:p1",
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},
		{
			name: "a moved pane keeps its agent's activity, even through a stale snapshot",
			steps: []step{
				{at: 1, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 2, ev: movedTo("w1:p9")},
				{at: 3, sync: &change{status: working("w1:p3")}, taken: sec(1)},
			},
			watched: "w1:p1",
			want:    "w1:p9", rule: herdr.ChosenBySameTab,
		},

		// Agents and panes that go away.
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
			name:    "a pane whose process exited loses its agent",
			steps:   []step{{at: 1, ev: exited("w1:p3")}},
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
			name:    "a status event for a pane with no agent changes nothing",
			steps:   []step{{at: 1, ev: left("w1:p5")}},
			watched: "w1:p5",
			want:    "w1:p4", rule: herdr.ChosenBySameTab,
		},
		{
			name: "a new agent in a pane doesn't inherit the old one's kind",
			steps: []step{{at: 1, ev: &herdr.Event{
				Kind: herdr.EventAgentStatusChanged, PaneID: "w1:p3", WorkspaceID: "w1",
				AgentStatus: herdr.AgentIdle, Agent: "claude",
			}}},
			watched:   "w1:p1",
			pins:      []config.Pin{{PaneTitle: "web", Agent: "Codex"}},
			ask:       herdr.AskTie,
			pinField:  herdr.PinByLabel,
			unmatched: &config.Pin{PaneTitle: "web", Agent: "Codex"},
		},

		// Blocked agents.
		{
			name: "a blocked agent is skipped even if it's the most recent",
			steps: []step{
				{at: 1, ev: status("w1:p3", herdr.AgentWorking)},
				{at: 2, ev: status("w1:p3", herdr.AgentBlocked)},
			},
			watched: "w1:p1",
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "a tab with only blocked agents asks",
			watched: "w2:p2",
			ask:     herdr.AskOnlyBlocked,
		},
		{
			name:    "a pin can still name a blocked agent, for delivery to refuse or confirm",
			watched: "w2:p2",
			pins:    []config.Pin{{PaneTitle: "hugo server", Agent: "claude"}},
			want:    "w2:p1", rule: herdr.ChosenByPin, pinField: herdr.PinByTerminalTitle,
		},

		// Pins.
		{
			name:    "a pin by kind beats activity",
			steps:   []step{{at: 1, ev: status("w1:p2", herdr.AgentWorking)}},
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "codex"}},
			want:    "w1:p3", rule: herdr.ChosenByPin, pinField: herdr.PinByLabel,
		},
		{
			name:    "a pin matches the kind herdr shows, ignoring case",
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "CODEX"}},
			want:    "w1:p3", rule: herdr.ChosenByPin, pinField: herdr.PinByLabel,
		},
		{
			name:    "a pin by herdr name can reach another tab",
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "reviewer"}},
			want:    "w1:p4", rule: herdr.ChosenByPin, pinField: herdr.PinByLabel,
		},
		{
			name:     "a pin by pane label only in another workspace asks",
			watched:  "w1:p1",
			pins:     []config.Pin{{PaneTitle: "web", Agent: "claude-docs"}},
			ask:      herdr.AskPinElsewhere,
			pinField: herdr.PinByLabel,
		},
		{
			name:     "a pin by kind only in other tabs asks",
			steps:    []step{{at: 1, ev: status("w1:p4", herdr.AgentWorking)}},
			watched:  "w1:p6",
			pins:     []config.Pin{{PaneTitle: "swm@gollum:~/src/worker", Agent: "claude"}},
			ask:      herdr.AskPinElsewhere,
			pinField: herdr.PinByTerminalTitle,
		},
		{
			name:    "a pin by kind with several matches takes the one in the tab",
			watched: "w1:p5",
			pins:    []config.Pin{{PaneTitle: "api", Agent: "claude"}},
			want:    "w1:p4", rule: herdr.ChosenByPin, pinField: herdr.PinByLabel,
		},
		{
			name:    "a terminal title doesn't count when the pane has a label",
			steps:   []step{{at: 1, ev: status("w1:p2", herdr.AgentWorking)}},
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "next dev", Agent: "codex"}},
			want:    "w1:p2", rule: herdr.ChosenBySameTab,
		},
		{
			name:    "the first pin that names the pane decides",
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "reviewer"}, {PaneTitle: "web", Agent: "claude"}},
			want:    "w1:p4", rule: herdr.ChosenByPin, pinField: herdr.PinByLabel,
		},
		{
			name:    "a pin for an agent that isn't there falls back to the tab and says so",
			steps:   []step{{at: 1, ev: status("w1:p3", herdr.AgentWorking)}},
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "web", Agent: "gemini"}, {PaneTitle: "web", Agent: "reviewer"}},
			want:    "w1:p3", rule: herdr.ChosenBySameTab, pinField: herdr.PinByLabel,
			unmatched: &config.Pin{PaneTitle: "web", Agent: "gemini"},
		},
		{
			name:      "a pin for an agent that isn't there still asks when the tab is empty",
			watched:   "w1:p6",
			pins:      []config.Pin{{PaneTitle: "swm@gollum:~/src/worker", Agent: "gemini"}},
			ask:       herdr.AskNoAgentInTab,
			pinField:  herdr.PinByTerminalTitle,
			unmatched: &config.Pin{PaneTitle: "swm@gollum:~/src/worker", Agent: "gemini"},
		},
		{
			name:    "a pin for another pane is ignored",
			steps:   []step{{at: 1, ev: status("w1:p3", herdr.AgentWorking)}},
			watched: "w1:p1",
			pins:    []config.Pin{{PaneTitle: "api", Agent: "reviewer"}},
			want:    "w1:p3", rule: herdr.ChosenBySameTab,
		},

		// The watched pane.
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
			name:    "an agent in the watched pane itself doesn't fill its tab",
			steps:   []step{{at: 1, ev: status("w1:p6", herdr.AgentWorking)}},
			watched: "w1:p6",
			ask:     herdr.AskNoAgentInTab,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := herdr.NewAgentTracker()
			tracker.Sync(tt.initial.apply(snap), t0)
			play(t, tracker, snap, tt.steps)

			got := tracker.Choose(tt.watched, tt.pins)
			if got.Chosen() != (tt.want != "") {
				t.Fatalf("Choose = %+v, want chosen %v", got, tt.want != "")
			}
			if got.Target.PaneID != tt.want || got.Rule != tt.rule || got.Ask != tt.ask {
				t.Errorf("Choose = %s by %q, ask %q; want %s by %q, ask %q",
					got.Target.PaneID, got.Rule, got.Ask, tt.want, tt.rule, tt.ask)
			}
			if got.PinField != tt.pinField || (got.Pin == nil) != (tt.pinField == "") {
				t.Errorf("pin = %+v by %q, want one by %q", got.Pin, got.PinField, tt.pinField)
			}
			if !reflect.DeepEqual(got.UnmatchedPin, tt.unmatched) {
				t.Errorf("UnmatchedPin = %+v, want %+v", got.UnmatchedPin, tt.unmatched)
			}
		})
	}
}

// herdr shouldn't send a pane without an ID, but if it does it mustn't turn
// into an agent with no target.
func TestTrackerIgnoresEmptyPaneIDs(t *testing.T) {
	snap := loadSnapshot(t, "two-workspaces.json")
	snap.Panes = append(snap.Panes, herdr.Pane{TabID: "w1:t1", Agent: "claude", AgentStatus: herdr.AgentWorking})
	snap.Agents = append(snap.Agents, herdr.AgentInfo{TabID: "w1:t1", Agent: "claude", Status: herdr.AgentWorking})
	tracker := herdr.NewAgentTracker()
	tracker.Sync(snap, t0)
	tracker.Observe(*status("", herdr.AgentWorking), at(1))
	tracker.Observe(*closed(""), at(2))

	got := tracker.Candidates("w1:p1")
	if len(got) != 4 {
		t.Fatalf("Candidates = %+v, want the fixture's 4 agents", got)
	}
	for _, c := range got {
		if c.PaneID == "" {
			t.Errorf("a candidate has no pane ID: %+v", c)
		}
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
			steps:   []step{{at: 1, ev: status("w1:p4", herdr.AgentWorking)}},
			watched: "w1:p6",
			want:    []string{"reviewer (tab 2)", "Codex (tab 1)", "claude (tab 1)", "claude-docs (workspace w2, tab 1)"},
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
