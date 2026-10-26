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
	// more recent change. After herdr restores a session the agents get
	// consecutive numbers in restore order, so until an agent changes state
	// its number says nothing about how busy it has been.
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

// AgentSnapshot is what herdr said about panes, agents and tabs at one
// moment. Any part can be missing. An agent that turns up in Panes but not in
// Agents still counts, it just has no name or state change counter.
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
//
// Choose and Candidates hand back a Target whatever its Status. Delivery must
// refuse, or ask the user to confirm, when Status is blocked: that usually
// means a permission dialog, and a prompt types text and presses Enter into
// it. That holds for a pinned or hand picked target too.
type Target struct {
	// PaneID is what agent.prompt takes as its target. herdr also accepts an
	// agent's name, but names are optional and follow the agent rather than
	// the pane. herdr's guide says closed pane IDs aren't reused, but they
	// look like counters per workspace and that hasn't been checked. So
	// delivery should look the pane up again just before sending and make
	// sure the same kind of agent is still in it.
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

// PinField says which of the watched pane's fields a pin's pane_title matched.
type PinField string

// The fields a pin can match.
const (
	PinByLabel         PinField = "label"
	PinByTerminalTitle PinField = "terminal title"
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
	// AskOnlyBlocked means every agent in the tab is blocked, which usually
	// means it's waiting on a permission dialog.
	AskOnlyBlocked AskReason = "every agent in this tab is waiting on a prompt"
	// AskTie means more than one agent in the tab could be the one, and
	// nothing tells them apart.
	AskTie AskReason = "no agent in this tab has started work since Sightline started"
	// AskPinElsewhere means a pin's agent only matched by label or kind in
	// another tab. Only a herdr name, which is unique, may pick an agent
	// outside the tab.
	AskPinElsewhere AskReason = "the pinned agent is only in another tab"
)

// Choice is the result of Choose: an agent, or a reason to ask.
type Choice struct {
	// Target is the agent to send to. It's only meaningful when Ask is empty.
	Target Target
	// Rule says how Target was picked.
	Rule ChoiceRule
	// Ask is why no agent was picked. The feed should ask with Candidates.
	Ask AskReason
	// Pin is the pin that named the watched pane, if any, and PinField which
	// of the pane's fields it matched, so the feed can say "pinned by terminal
	// title".
	Pin      *config.Pin
	PinField PinField
	// UnmatchedPin is set when a pin named the watched pane but no live agent
	// fitted its agent anywhere, so the choice fell through to the tab. The
	// feed can say so.
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
// An agent counts as active when it starts working: a change into working
// from idle, done or unknown, or already working the first time it's seen.
// That's the moment an agent picks up a task, so it's the best sign of the one
// that just changed the code. Going idle, done or blocked isn't new activity,
// and nor is blocked to working, which is someone approving a prompt.
//
// On herdr 0.9.1 pane.agent_status_changed has been seen to stay silent while
// an agent went idle and working many times, so events can't be relied on.
// Sync works activity out by itself from one agent.list to the next, so the
// owner should Sync every few seconds. Events, where they arrive, only make it
// quicker. Sync again on pane.agent_detected and after EventReconnected,
// Observe pane.closed, pane.exited and pane.moved, and stamp each snapshot
// with when its fetch began (AgentSnapshot.Taken) so a slow one can't undo
// events that overtook it.
type AgentTracker struct {
	mu     sync.Mutex
	panes  map[string]Pane
	tabs   map[string]Tab
	agents map[string]*trackedAgent
	// closed is when an event said each pane closed, exited or moved away,
	// and left when an event said its agent left. Sync uses them to stop an
	// older snapshot bringing the pane or agent back.
	closed map[string]time.Time
	left   map[string]time.Time
}

type trackedAgent struct {
	info AgentInfo
	// active is when the agent last started working. Zero means Sightline
	// hasn't seen it start.
	active time.Time
	// heard is when an event last told the tracker about the agent.
	heard time.Time
	// seqTrusted says info.StateChangeSeq matches info.Status. It's false
	// once an event has changed the status, because events don't carry the
	// counter, and true again after the next snapshot.
	seqTrusted bool
}

// NewAgentTracker returns a tracker that knows about no agents yet.
func NewAgentTracker() *AgentTracker {
	return &AgentTracker{
		panes:  map[string]Pane{},
		tabs:   map[string]Tab{},
		agents: map[string]*trackedAgent{},
		closed: map[string]time.Time{},
		left:   map[string]time.Time{},
	}
}

// startsWork reports whether going from one status to another is new
// activity.
func startsWork(from, to AgentStatus) bool {
	return to == AgentWorking && (from == AgentIdle || from == AgentDone || from == AgentUnknown || from == "")
}

// Sync replaces what the tracker knows with a fresh snapshot, applied at now.
// Agents missing from it are forgotten. Agents it still has keep their
// activity, and gain new activity if the snapshot shows they started work
// since the last one. Where an event has said something more recent than the
// snapshot (see AgentSnapshot.Taken), the event wins.
func (t *AgentTracker) Sync(snap AgentSnapshot, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	newer := func(heard time.Time) bool {
		return !snap.Taken.IsZero() && !heard.IsZero() && !heard.Before(snap.Taken)
	}

	t.panes = make(map[string]Pane, len(snap.Panes))
	for _, p := range snap.Panes {
		if p.ID != "" && !newer(t.closed[p.ID]) {
			t.panes[p.ID] = p
		}
	}
	t.tabs = make(map[string]Tab, len(snap.Tabs))
	for _, tab := range snap.Tabs {
		t.tabs[tab.ID] = tab
	}

	infos := make(map[string]AgentInfo, len(snap.Agents))
	for _, a := range snap.Agents {
		if a.PaneID != "" && (a.Agent != "" || a.DisplayAgent != "") {
			infos[a.PaneID] = a
		}
	}
	for _, p := range snap.Panes {
		if _, ok := infos[p.ID]; ok || p.ID == "" || (p.Agent == "" && p.DisplayAgent == "") {
			continue
		}
		infos[p.ID] = AgentInfo{
			PaneID: p.ID, WorkspaceID: p.WorkspaceID, TabID: p.TabID,
			Agent: p.Agent, DisplayAgent: p.DisplayAgent, Status: p.AgentStatus,
		}
	}

	old := t.agents
	t.agents = make(map[string]*trackedAgent, len(infos))
	for id, info := range infos {
		if newer(t.closed[id]) || newer(t.left[id]) {
			continue
		}
		a := &trackedAgent{info: info}
		prev, seen := old[id]
		switch {
		case seen && newer(prev.heard):
			// The snapshot's names, tab and counter are still the best there
			// are, but the event knows the status better.
			if prev.info.Agent != "" && prev.info.Agent != info.Agent {
				a.info.Agent = prev.info.Agent
				a.info.Name, a.info.DisplayAgent = "", ""
			}
			a.info.Status = prev.info.Status
			a.active, a.heard = prev.active, prev.heard
		case seen:
			a.active = prev.active
			a.seqTrusted = info.StateChangeSeq != 0
			// The counter moving while the agent was working and still is
			// means it stopped and started again between snapshots.
			restarted := prev.seqTrusted && prev.info.Status == AgentWorking &&
				info.Status == AgentWorking && info.StateChangeSeq > prev.info.StateChangeSeq
			if startsWork(prev.info.Status, info.Status) || restarted {
				a.active = now
			}
		default:
			a.seqTrusted = info.StateChangeSeq != 0
			if info.Status == AgentWorking {
				a.active = now
			}
		}
		t.agents[id] = a
	}
	for id, prev := range old {
		if _, ok := t.agents[id]; !ok && newer(prev.heard) {
			t.agents[id] = prev
		}
	}
	for _, m := range []map[string]time.Time{t.closed, t.left} {
		for id, when := range m {
			if !newer(when) {
				delete(m, id)
			}
		}
	}
}

// Observe updates the tracker from one event herdr sent at now. Events it has
// no use for are ignored.
func (t *AgentTracker) Observe(ev Event, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if ev.PaneID == "" {
		return
	}
	switch ev.Kind {
	case EventAgentStatusChanged:
		t.statusChanged(ev, now)
	case EventPaneClosed, EventPaneExited:
		t.closed[ev.PaneID] = now
		delete(t.agents, ev.PaneID)
		delete(t.panes, ev.PaneID)
	case EventPaneMoved:
		t.moved(ev, now)
	}
}

func (t *AgentTracker) statusChanged(ev Event, now time.Time) {
	a, ok := t.agents[ev.PaneID]
	if ev.Agent == "" && ev.AgentStatus == AgentUnknown {
		// The agent has left the pane.
		t.left[ev.PaneID] = now
		delete(t.agents, ev.PaneID)
		return
	}
	if !ok && ev.Agent == "" {
		// Not an agent the tracker knows, and the event doesn't say what it
		// is. The next Sync will tell.
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
	if startsWork(a.info.Status, ev.AgentStatus) {
		a.active = now
	}
	a.info.Status = ev.AgentStatus
	a.heard = now
	a.seqTrusted = false
	delete(t.left, ev.PaneID)
}

// moved follows a pane to its new ID. Its agent keeps its activity.
func (t *AgentTracker) moved(ev Event, now time.Time) {
	if ev.PreviousPaneID == "" || ev.Pane == nil {
		return
	}
	from, to := ev.PreviousPaneID, ev.PaneID
	t.closed[from] = now
	delete(t.panes, from)
	t.panes[to] = *ev.Pane

	a, ok := t.agents[from]
	delete(t.agents, from)
	if !ok {
		return
	}
	a.info.PaneID = to
	a.info.TabID = ev.Pane.TabID
	a.info.WorkspaceID = ev.Pane.WorkspaceID
	a.heard = now
	t.agents[to] = a
}

// Choose picks the agent to send an error from the watched pane to.
//
//  1. The first pin whose pane_title names the watched pane decides. It
//     matches the pane's label, or its terminal title when the pane has no
//     label. Only an agent's herdr name, which is unique, can pick an agent in
//     another tab. A label or kind match must be in the watched pane's tab,
//     and if it's only found elsewhere Choose asks. See matchPin.
//  2. Otherwise the most recently active agent in the watched pane's tab that
//     isn't blocked. If more than one could be the one and nothing tells them
//     apart, it asks.
//  3. Otherwise it asks. It never falls back to another tab.
//
// The watched pane itself is never chosen, even if it's an agent, so an
// agent's own errors don't loop back to it.
//
// A pinned target can be blocked. Delivery must refuse or confirm then (see
// Target). When Choose asks, the feed must wait for the user to pick from
// Candidates and never fall back to the first candidate without a keypress.
func (t *AgentTracker) Choose(watchedPaneID string, pins []config.Pin) Choice {
	t.mu.Lock()
	defer t.mu.Unlock()

	watched, ok := t.panes[watchedPaneID]
	if !ok || watched.TabID == "" {
		return Choice{Ask: AskUnknownPane}
	}
	ranked := t.rank(watched)
	var sameTab []*trackedAgent
	for _, a := range ranked {
		if a.info.TabID == watched.TabID {
			sameTab = append(sameTab, a)
		}
	}

	var choice Choice
	if pin, field := pinFor(watched, pins); pin != nil {
		choice.Pin, choice.PinField = pin, field
		a, ask := t.matchPin(ranked, sameTab, pin.Agent)
		switch {
		case a != nil:
			choice.Target, choice.Rule = t.target(a, watched), ChosenByPin
			return choice
		case ask != "":
			choice.Ask = ask
			return choice
		}
		choice.UnmatchedPin = pin
	}

	var ready []*trackedAgent
	for _, a := range sameTab {
		if a.info.Status != AgentBlocked {
			ready = append(ready, a)
		}
	}
	switch {
	case len(ready) == 0 && len(sameTab) > 0:
		choice.Ask = AskOnlyBlocked
	case len(ready) == 0:
		choice.Ask = AskNoAgentInTab
	case len(ready) > 1 && ready[0].active.Equal(ready[1].active):
		choice.Ask = AskTie
	default:
		choice.Target, choice.Rule = t.target(ready[0], watched), ChosenBySameTab
	}
	return choice
}

// pinFor returns the first pin that names the watched pane and which field it
// matched. The terminal title only counts when the pane has no label, because
// the process in the pane can set its title to anything.
func pinFor(watched Pane, pins []config.Pin) (*config.Pin, PinField) {
	for i, pin := range pins {
		switch {
		case pin.PaneTitle == "":
		case watched.Label != "":
			if pin.PaneTitle == watched.Label {
				return &pins[i], PinByLabel
			}
		case pin.PaneTitle == watched.Title:
			return &pins[i], PinByTerminalTitle
		}
	}
	return nil, ""
}

// matchPin finds the agent a pin's agent names, ignoring case. It tries the
// agent's herdr name first, anywhere, since names are unique. Then the label
// on its pane, then its kind as herdr shows it (display_agent) or detected it
// (agent, such as claude), both only in the watched pane's tab. If several
// agents in the tab fit, the one earliest in ranked wins. If nothing in the
// tab fits but a label or kind does elsewhere, it says to ask. If nothing fits
// at all it returns neither.
func (t *AgentTracker) matchPin(ranked, sameTab []*trackedAgent, want string) (*trackedAgent, AskReason) {
	if want == "" {
		return nil, ""
	}
	for _, a := range ranked {
		if strings.EqualFold(a.info.Name, want) {
			return a, ""
		}
	}
	fits := []func(*trackedAgent) bool{
		func(a *trackedAgent) bool { return strings.EqualFold(t.panes[a.info.PaneID].Label, want) },
		func(a *trackedAgent) bool {
			return strings.EqualFold(a.info.DisplayAgent, want) || strings.EqualFold(a.info.Agent, want)
		},
	}
	for _, fit := range fits {
		for _, a := range sameTab {
			if fit(a) {
				return a, ""
			}
		}
	}
	for _, fit := range fits {
		for _, a := range ranked {
			if fit(a) {
				return nil, AskPinElsewhere
			}
		}
	}
	return nil, ""
}

// Candidates lists every agent the watched pane's errors could go to, for the
// feed to offer. Agents in the same tab come first, then the rest, each most
// recently active first and then by herdr's state change counter. Blocked
// agents are listed too, with their Status. The watched pane itself is left
// out. If the watched pane isn't known, every agent is listed by activity
// alone.
//
// The order is a suggestion for the picker. Nothing should be sent to the
// first candidate without the user choosing it.
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
// most recently active, then the state change counter, then pane ID so the
// order never wobbles.
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
