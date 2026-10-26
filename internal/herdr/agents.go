package herdr

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/swmcc/herdr-sightline/internal/config"
)

// AgentInfo is one agent as agent.list describes it. It carries two things
// pane.list doesn't: the agent's name and herdr's state change counter.
type AgentInfo struct {
	PaneID       string `json:"pane_id"`
	WorkspaceID  string `json:"workspace_id"`
	TabID        string `json:"tab_id"`
	Agent        string `json:"agent"`
	DisplayAgent string `json:"display_agent"`
	// Name is set with herdr agent rename. It's unique among live agents but
	// usually empty.
	Name   string      `json:"name"`
	Status AgentStatus `json:"agent_status"`
	// StateChangeSeq is a counter herdr keeps across the whole server and
	// stamps on an agent each time its state changes. A higher number means a
	// more recent change, which is the only history herdr offers about agents
	// Sightline hasn't been watching.
	StateChangeSeq uint64 `json:"state_change_seq"`
}

// ListAgents returns every pane herdr has detected an agent in.
func (c *Client) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	var out struct {
		Agents []AgentInfo `json:"agents"`
	}
	if err := c.call(ctx, "agent.list", struct{}{}, "agent_list", &out); err != nil {
		return nil, err
	}
	return out.Agents, nil
}

// Tab is one tab as tab.list describes it.
type Tab struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	// Number is what herdr shows for the tab in its workspace.
	Number int    `json:"number"`
	Label  string `json:"label"`
}

// ListTabs returns every tab in every workspace.
func (c *Client) ListTabs(ctx context.Context) ([]Tab, error) {
	var out struct {
		Tabs []Tab `json:"tabs"`
	}
	if err := c.call(ctx, "tab.list", struct{}{}, "tab_list", &out); err != nil {
		return nil, err
	}
	return out.Tabs, nil
}

// AgentSnapshot is what herdr said about panes, agents and tabs at one moment. Any
// part can be missing. An agent that turns up in Panes but not in Agents still
// counts, it just has no name or state change counter.
type AgentSnapshot struct {
	Panes  []Pane
	Agents []AgentInfo
	Tabs   []Tab
	// Taken is when the first of the lists was asked for. Fetching them takes
	// a few round trips, so events can overtake a snapshot. Anything heard by
	// event at or after Taken is newer than the snapshot and wins over it.
	// Leave it zero to trust the snapshot over every event.
	Taken time.Time
}

// Target is an agent Sightline could send an error to.
type Target struct {
	// PaneID is what agent.prompt takes as its target. herdr also accepts an
	// agent's name, but names are optional and follow the agent rather than the
	// pane. A pane ID is always there and is never reused, so if the pane moves
	// or closes a prompt fails instead of reaching someone else.
	PaneID      string
	WorkspaceID string
	TabID       string
	// TabNumber is the tab's number in herdr, or 0 if it isn't known.
	TabNumber int
	// Kind is the kind of agent herdr detected, such as claude or codex.
	Kind string
	// Name is what to call the agent: its herdr name, else the pane's label,
	// else the kind herdr shows.
	Name   string
	Status AgentStatus
	// SameTab says whether the agent shares a tab with the watched pane it was
	// worked out for.
	SameTab bool
	// Label is how the feed shows the agent, for example "claude (tab 2)". An
	// agent in another workspace says which, because tab numbers start again
	// in every workspace.
	Label string
}

// ChoiceRule says how Choose picked an agent.
type ChoiceRule string

// The rules Choose follows, in order.
const (
	ChosenByPin     ChoiceRule = "pinned in sightline.toml"
	ChosenBySameTab ChoiceRule = "most recently active in this tab"
)

// AskReason says why Choose wouldn't pick an agent.
type AskReason string

// The reasons Choose asks instead of choosing.
const (
	// AskUnknownPane means the watched pane wasn't in the last snapshot, so
	// there's no tab to look in.
	AskUnknownPane AskReason = "the watched pane isn't known to herdr"
	// AskNoAgentInTab means nothing in the watched pane's tab is an agent.
	// Sightline never quietly reaches into another tab.
	AskNoAgentInTab AskReason = "no agent in this tab"
)

// Choice is the result of Choose: an agent, or a reason to ask.
type Choice struct {
	// Target is the agent to send to. It's only meaningful when Ask is empty.
	Target Target
	// Rule says how Target was picked.
	Rule ChoiceRule
	// Ask is why no agent was picked. The feed should ask with Candidates.
	Ask AskReason
	// UnmatchedPin is set when a pin named the watched pane but no live agent
	// fitted its agent, so the choice fell through to the tab. The feed can
	// say so.
	UnmatchedPin *config.Pin
}

// Chosen reports whether Choose picked an agent.
func (c Choice) Chosen() bool { return c.Ask == "" }

// AgentTracker keeps track of the agents herdr knows about and when each was
// last active, so Sightline can pick one to send an error to.
//
// It has no goroutine of its own. Whoever owns the herdr connection feeds it
// snapshots with Sync and events with Observe, and it's safe to use from
// several goroutines at once.
//
// Activity means an agent starting to work: a change into working, from any
// other status. That's the moment an agent picks up a task, so it's the best
// sign of the one that just changed the code. Going idle, done or blocked
// isn't new activity. An agent already working the first time it's seen
// counts as active then. Agents never seen starting work are ranked by
// herdr's state change counter instead (see AgentInfo.StateChangeSeq).
//
// Agents report status through pane.agent_status_changed, which herdr only
// sends per pane. Subscribe to it for each agent pane, Sync again on
// pane.agent_detected and after EventReconnected, and Observe pane.closed.
// Stamp each snapshot with when its fetch began (AgentSnapshot.Taken) so a
// slow one can't undo events that overtook it.
type AgentTracker struct {
	mu     sync.Mutex
	panes  map[string]Pane
	tabs   map[string]Tab
	agents map[string]*trackedAgent
	// gone is when an event said an agent left each pane. Sync uses it to
	// stop an older snapshot bringing the agent back.
	gone map[string]time.Time
}

type trackedAgent struct {
	info AgentInfo
	// active is when the agent last started working. Zero means Sightline
	// hasn't seen it start.
	active time.Time
	// heard is when an event last told the tracker about the agent.
	heard time.Time
}

// NewAgentTracker returns a tracker that knows about no agents yet.
func NewAgentTracker() *AgentTracker {
	return &AgentTracker{
		panes:  map[string]Pane{},
		tabs:   map[string]Tab{},
		agents: map[string]*trackedAgent{},
		gone:   map[string]time.Time{},
	}
}

// Sync replaces what the tracker knows with a fresh snapshot, applied at now.
// Agents missing from it are forgotten. Agents it still has keep their
// activity. Where an event has said something more recent than the snapshot
// (see AgentSnapshot.Taken), the event wins.
func (t *AgentTracker) Sync(snap AgentSnapshot, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.panes = make(map[string]Pane, len(snap.Panes))
	for _, p := range snap.Panes {
		t.panes[p.ID] = p
	}
	t.tabs = make(map[string]Tab, len(snap.Tabs))
	for _, tab := range snap.Tabs {
		t.tabs[tab.ID] = tab
	}

	infos := make(map[string]AgentInfo, len(snap.Agents))
	for _, a := range snap.Agents {
		if a.Agent != "" || a.DisplayAgent != "" {
			infos[a.PaneID] = a
		}
	}
	for _, p := range snap.Panes {
		if _, ok := infos[p.ID]; ok || (p.Agent == "" && p.DisplayAgent == "") {
			continue
		}
		infos[p.ID] = AgentInfo{
			PaneID: p.ID, WorkspaceID: p.WorkspaceID, TabID: p.TabID,
			Agent: p.Agent, DisplayAgent: p.DisplayAgent, Status: p.AgentStatus,
		}
	}

	newer := func(heard time.Time) bool {
		return !snap.Taken.IsZero() && !heard.IsZero() && !heard.Before(snap.Taken)
	}
	old := t.agents
	t.agents = make(map[string]*trackedAgent, len(infos))
	for id, info := range infos {
		if newer(t.gone[id]) {
			continue
		}
		a := &trackedAgent{info: info}
		prev, seen := old[id]
		if seen && newer(prev.heard) {
			// The snapshot's names, tab and counter are still the best there
			// are, but the event knows the status better.
			a.info.Agent = cmp.Or(prev.info.Agent, info.Agent)
			a.info.Status = prev.info.Status
			a.active, a.heard = prev.active, prev.heard
			t.agents[id] = a
			continue
		}
		if seen {
			a.active = prev.active
		}
		if info.Status == AgentWorking && (!seen || prev.info.Status != AgentWorking) {
			a.active = now
		}
		t.agents[id] = a
	}
	for id, prev := range old {
		if _, ok := t.agents[id]; !ok && newer(prev.heard) {
			t.agents[id] = prev
		}
	}
	for id, when := range t.gone {
		if !newer(when) {
			delete(t.gone, id)
		}
	}
}

// Observe updates the tracker from one event herdr sent at now. Events it has
// no use for are ignored.
func (t *AgentTracker) Observe(ev Event, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	switch ev.Kind {
	case EventAgentStatusChanged:
		t.statusChanged(ev, now)
	case EventPaneClosed, EventPaneExited:
		t.forget(ev.PaneID, now)
		if ev.Kind == EventPaneClosed {
			delete(t.panes, ev.PaneID)
		}
	}
}

func (t *AgentTracker) statusChanged(ev Event, now time.Time) {
	a, ok := t.agents[ev.PaneID]
	if ev.Agent == "" && (!ok || ev.AgentStatus == AgentUnknown) {
		// The agent has left the pane, or this is a pane that never had one.
		t.forget(ev.PaneID, now)
		return
	}
	if !ok {
		// An agent the last snapshot didn't have. Fill in what's known now
		// and let the next Sync fill in the rest.
		a = &trackedAgent{info: AgentInfo{PaneID: ev.PaneID, WorkspaceID: ev.WorkspaceID}}
		if p, ok := t.panes[ev.PaneID]; ok {
			a.info.TabID = p.TabID
		}
		t.agents[ev.PaneID] = a
	}
	if ev.Agent != "" && ev.Agent != a.info.Agent {
		// A different agent has taken over the pane. The old one's name and
		// display kind don't belong to it.
		a.info.Agent = ev.Agent
		a.info.DisplayAgent, a.info.Name = "", ""
	}
	if ev.AgentStatus == AgentWorking && (!ok || a.info.Status != AgentWorking) {
		a.active = now
	}
	a.info.Status = ev.AgentStatus
	a.heard = now
	delete(t.gone, ev.PaneID)
}

func (t *AgentTracker) forget(paneID string, now time.Time) {
	if _, ok := t.agents[paneID]; ok {
		t.gone[paneID] = now
	}
	delete(t.agents, paneID)
}

// Choose picks the agent to send an error from the watched pane to.
//
//  1. The first pin whose pane_title is the watched pane's label or terminal
//     title, exactly, decides if a live agent fits its agent. See matchPin.
//  2. Otherwise the most recently active agent in the watched pane's tab.
//  3. Otherwise it asks. It never falls back to another tab.
//
// The watched pane itself is never chosen, even if it's an agent, so an
// agent's own errors don't loop back to it.
func (t *AgentTracker) Choose(watchedPaneID string, pins []config.Pin) Choice {
	t.mu.Lock()
	defer t.mu.Unlock()

	watched, ok := t.panes[watchedPaneID]
	if !ok {
		return Choice{Ask: AskUnknownPane}
	}
	ranked := t.rank(watched)

	var unmatched *config.Pin
	for _, pin := range pins {
		if pin.PaneTitle == "" || (pin.PaneTitle != watched.Label && pin.PaneTitle != watched.Title) {
			continue
		}
		if a := t.matchPin(ranked, pin.Agent); a != nil {
			return Choice{Target: t.target(a, watched), Rule: ChosenByPin}
		}
		unmatched = &pin
		break
	}

	if len(ranked) > 0 && watched.TabID != "" && ranked[0].info.TabID == watched.TabID {
		return Choice{Target: t.target(ranked[0], watched), Rule: ChosenBySameTab, UnmatchedPin: unmatched}
	}
	return Choice{Ask: AskNoAgentInTab, UnmatchedPin: unmatched}
}

// matchPin finds the agent a pin's agent names, ignoring case. It tries the
// agent's herdr name first, then the label on its pane, then its kind as herdr
// shows it (display_agent) or detected it (agent, such as claude). The first
// of those that any agent has wins, and if several agents share it the one
// earliest in ranked does.
func (t *AgentTracker) matchPin(ranked []*trackedAgent, want string) *trackedAgent {
	if want == "" {
		return nil
	}
	fits := []func(*trackedAgent) bool{
		func(a *trackedAgent) bool { return strings.EqualFold(a.info.Name, want) },
		func(a *trackedAgent) bool { return strings.EqualFold(t.panes[a.info.PaneID].Label, want) },
		func(a *trackedAgent) bool {
			return strings.EqualFold(a.info.DisplayAgent, want) || strings.EqualFold(a.info.Agent, want)
		},
	}
	for _, fit := range fits {
		for _, a := range ranked {
			if fit(a) {
				return a
			}
		}
	}
	return nil
}

// Candidates lists every agent the watched pane's errors could go to, for the
// feed to offer. Agents in the same tab come first, then the rest, each most
// recently active first. The watched pane itself is left out. If the watched
// pane isn't known, every agent is listed by activity alone.
func (t *AgentTracker) Candidates(watchedPaneID string) []Target {
	t.mu.Lock()
	defer t.mu.Unlock()

	watched := t.panes[watchedPaneID]
	watched.ID = watchedPaneID
	ranked := t.rank(watched)
	out := make([]Target, len(ranked))
	for i, a := range ranked {
		out[i] = t.target(a, watched)
	}
	return out
}

// rank orders the agents other than the watched pane: same tab first, then
// most recently active, then pane ID so the order never wobbles.
func (t *AgentTracker) rank(watched Pane) []*trackedAgent {
	ranked := make([]*trackedAgent, 0, len(t.agents))
	for id, a := range t.agents {
		if id != watched.ID {
			ranked = append(ranked, a)
		}
	}
	sameTab := func(a *trackedAgent) bool { return watched.TabID != "" && a.info.TabID == watched.TabID }
	slices.SortFunc(ranked, func(a, b *trackedAgent) int {
		if sa, sb := sameTab(a), sameTab(b); sa != sb {
			if sa {
				return -1
			}
			return 1
		}
		if c := b.active.Compare(a.active); c != 0 {
			return c
		}
		if c := cmp.Compare(b.info.StateChangeSeq, a.info.StateChangeSeq); c != 0 {
			return c
		}
		return cmp.Compare(a.info.PaneID, b.info.PaneID)
	})
	return ranked
}

func (t *AgentTracker) target(a *trackedAgent, watched Pane) Target {
	tab := t.tabs[a.info.TabID]
	name := cmp.Or(a.info.Name, t.panes[a.info.PaneID].Label, a.info.DisplayAgent, a.info.Agent)
	target := Target{
		PaneID:      a.info.PaneID,
		WorkspaceID: a.info.WorkspaceID,
		TabID:       a.info.TabID,
		TabNumber:   tab.Number,
		Kind:        a.info.Agent,
		Name:        name,
		Status:      a.info.Status,
		SameTab:     watched.TabID != "" && a.info.TabID == watched.TabID,
	}
	target.Label = targetLabel(target, watched.WorkspaceID)
	return target
}

func targetLabel(t Target, watchedWorkspace string) string {
	var where []string
	if t.WorkspaceID != "" && t.WorkspaceID != watchedWorkspace {
		where = append(where, "workspace "+t.WorkspaceID)
	}
	if t.TabNumber > 0 {
		where = append(where, fmt.Sprintf("tab %d", t.TabNumber))
	}
	if len(where) == 0 {
		return t.Name
	}
	return fmt.Sprintf("%s (%s)", t.Name, strings.Join(where, ", "))
}
