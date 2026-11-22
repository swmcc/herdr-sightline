package herdrtest

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Pane is a pane in a fake Herdr. The tagged fields are what pane.list sends.
type Pane struct {
	ID          string `json:"pane_id"`
	TerminalID  string `json:"terminal_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	Focused     bool   `json:"focused"`
	Label       string `json:"label"`
	Cwd         string `json:"cwd"`
	// Agent is the agent herdr has detected in the pane, like "claude". Leave
	// it empty for a pane with no agent, such as a dev server.
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
	// AgentName is what herdr agent rename set. agent.list sends it as name.
	AgentName string `json:"-"`
	// Text is what pane.read returns: the pane's lines joined with newlines.
	Text string `json:"-"`
}

// Tab is a tab in a fake Herdr, as tab.list sends it.
type Tab struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
}

// Prompt is one agent.prompt a fake Herdr accepted.
type Prompt struct {
	Target string
	Text   string
}

// Herdr is a fake herdr with state. It answers ping, pane.list, pane.read,
// agent.list, tab.list and agent.prompt from the panes and tabs it holds, and
// pushes events to the subscriptions that asked for them. Anything else can
// still be set with Handle, as on a plain Server.
//
// It's safe to change from the test while a client is talking to it. Events go
// out after the change is made, so two goroutines changing the same pane at
// once can deliver their events in either order.
type Herdr struct {
	*Server

	mu      sync.Mutex
	version string
	panes   []Pane
	tabs    []Tab
	prompts []Prompt
	seq     map[string]uint64
	nextSeq uint64
	subs    []listener
}

// subscription is one entry in the subscriptions array of events.subscribe.
type subscription struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id"`
}

// listener is an open subscription and what it asked to hear about.
type listener struct {
	sub   *Subscriber
	wants []subscription
}

// NewHerdr starts a fake herdr with no panes and stops it when the test ends.
func NewHerdr(t testing.TB) *Herdr {
	t.Helper()
	h := &Herdr{Server: New(t), version: "0.9.1", seq: map[string]uint64{}}
	h.Handle("ping", h.ping)
	h.Handle("pane.list", h.paneList)
	h.Handle("pane.read", h.paneRead)
	h.Handle("agent.list", h.agentList)
	h.Handle("tab.list", h.tabList)
	h.Handle("agent.prompt", h.agentPrompt)
	h.Handle("events.subscribe", h.subscribe)
	h.Server.mu.Lock()
	h.Server.subscribed = h.subscribed
	h.Server.mu.Unlock()
	return h
}

// AddPane adds a pane, or replaces the one with the same ID.
func (h *Herdr) AddPane(p Pane) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i := h.find(p.ID); i >= 0 {
		changed := h.panes[i].Agent != p.Agent || h.panes[i].AgentStatus != p.AgentStatus
		h.panes[i] = p
		if changed {
			h.bump(p.ID)
		}
		return
	}
	h.panes = append(h.panes, p)
	h.bump(p.ID)
}

// AddTab adds a tab, or replaces the one with the same ID.
func (h *Herdr) AddTab(tab Tab) {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := slices.IndexFunc(h.tabs, func(t Tab) bool { return t.ID == tab.ID })
	if i >= 0 {
		h.tabs[i] = tab
		return
	}
	h.tabs = append(h.tabs, tab)
}

// Write appends lines to a pane, as a program running in it would. Each line
// starts a new line in the pane.
func (h *Herdr) Write(paneID string, lines ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := h.find(paneID)
	if i < 0 || len(lines) == 0 {
		return
	}
	p := &h.panes[i]
	text := strings.Join(lines, "\n")
	if p.Text == "" {
		p.Text = text
	} else {
		p.Text += "\n" + text
	}
}

// SetText replaces everything in a pane, as clearing the screen and drawing
// again would.
func (h *Herdr) SetText(paneID, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i := h.find(paneID); i >= 0 {
		h.panes[i].Text = text
	}
}

// SetAgentStatus changes the status of the agent in a pane and tells the
// subscriptions that asked, in the dotted envelope herdr uses for subscription
// events.
func (h *Herdr) SetAgentStatus(paneID, status string) {
	h.mu.Lock()
	i := h.find(paneID)
	if i < 0 {
		h.mu.Unlock()
		return
	}
	h.panes[i].AgentStatus = status
	h.bump(paneID)
	p := h.panes[i]
	subs := h.listening("pane.agent_status_changed", paneID)
	h.mu.Unlock()

	data := map[string]any{"pane_id": p.ID, "workspace_id": p.WorkspaceID, "agent_status": status, "agent": p.Agent}
	for _, sub := range subs {
		_ = sub.Push("pane.agent_status_changed", data)
	}
}

// ClosePane removes a pane and tells the subscriptions that asked, in the
// underscored envelope herdr uses for lifecycle events.
func (h *Herdr) ClosePane(paneID string) {
	h.mu.Lock()
	i := h.find(paneID)
	if i < 0 {
		h.mu.Unlock()
		return
	}
	p := h.panes[i]
	h.panes = slices.Delete(h.panes, i, i+1)
	delete(h.seq, paneID)
	subs := h.listening("pane.closed", paneID)
	h.mu.Unlock()

	for _, sub := range subs {
		_ = sub.PushLifecycle("pane_closed", map[string]any{"pane_id": p.ID, "workspace_id": p.WorkspaceID})
	}
}

// Restart drops every subscription, as herdr does when it restarts or hands
// off to a new server. The panes stay as they are.
func (h *Herdr) Restart() {
	h.mu.Lock()
	subs := h.subs
	h.subs = nil
	h.mu.Unlock()
	for _, l := range subs {
		l.sub.Drop()
	}
}

// Prompts returns every agent.prompt accepted so far, oldest first.
func (h *Herdr) Prompts() []Prompt {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.prompts)
}

// find returns the index of a pane, or -1. The caller holds mu.
func (h *Herdr) find(paneID string) int {
	return slices.IndexFunc(h.panes, func(p Pane) bool { return p.ID == paneID })
}

// bump stamps a pane's agent with the next state change number, as herdr does
// whenever an agent's state changes. The caller holds mu.
func (h *Herdr) bump(paneID string) {
	h.nextSeq++
	h.seq[paneID] = h.nextSeq
}

// listening returns the live subscriptions that asked for that event about paneID,
// forgetting any that have ended. The caller holds mu.
func (h *Herdr) listening(event, paneID string) []*Subscriber {
	h.subs = slices.DeleteFunc(h.subs, func(l listener) bool { return l.sub.ended() })
	var out []*Subscriber
	for _, l := range h.subs {
		if slices.ContainsFunc(l.wants, func(w subscription) bool {
			return w.Type == event && (w.PaneID == "" || w.PaneID == paneID)
		}) {
			out = append(out, l.sub)
		}
	}
	return out
}

func (h *Herdr) subscribed(sub *Subscriber) {
	var wants []subscription
	// A subscriptions array that doesn't parse wants nothing, so it hears
	// nothing.
	_ = json.Unmarshal(sub.Subscriptions, &wants)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs = append(h.subs, listener{sub: sub, wants: wants})
}

// subscribe refuses a subscription that names a pane that doesn't exist, as
// herdr does, so a pane closing fails the resubscribe after a restart.
func (h *Herdr) subscribe(req Request) (any, *ErrorBody) {
	var params struct {
		Subscriptions []subscription `json:"subscriptions"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || len(params.Subscriptions) == 0 {
		return nil, &ErrorBody{Code: "invalid_request", Message: "events.subscribe needs subscriptions"}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, w := range params.Subscriptions {
		if w.PaneID != "" && h.find(w.PaneID) < 0 {
			return nil, notFound(w.PaneID)
		}
	}
	return map[string]string{"type": "subscription_started"}, nil
}

func (h *Herdr) ping(Request) (any, *ErrorBody) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return map[string]any{"type": "pong", "version": h.version, "protocol": 1}, nil
}

func (h *Herdr) paneList(Request) (any, *ErrorBody) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return map[string]any{"type": "pane_list", "panes": slices.Clone(h.panes)}, nil
}

func (h *Herdr) paneRead(req Request) (any, *ErrorBody) {
	var params struct {
		PaneID string `json:"pane_id"`
		Lines  int    `json:"lines"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Lines < 1 {
		return nil, &ErrorBody{Code: "invalid_request", Message: "pane.read needs a pane_id and lines of at least 1"}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	i := h.find(params.PaneID)
	if i < 0 {
		return nil, notFound(params.PaneID)
	}
	p := h.panes[i]
	lines := strings.Split(p.Text, "\n")
	truncated := len(lines) > params.Lines
	if truncated {
		lines = lines[len(lines)-params.Lines:]
	}
	return map[string]any{"type": "pane_read", "read": map[string]any{
		"pane_id":      p.ID,
		"workspace_id": p.WorkspaceID,
		"tab_id":       p.TabID,
		"text":         strings.Join(lines, "\n"),
		"truncated":    truncated,
		"revision":     0,
	}}, nil
}

func (h *Herdr) agentList(Request) (any, *ErrorBody) {
	h.mu.Lock()
	defer h.mu.Unlock()
	agents := []map[string]any{}
	for _, p := range h.panes {
		if p.Agent == "" {
			continue
		}
		agents = append(agents, map[string]any{
			"pane_id":          p.ID,
			"workspace_id":     p.WorkspaceID,
			"tab_id":           p.TabID,
			"agent":            p.Agent,
			"display_agent":    p.Agent,
			"name":             p.AgentName,
			"agent_status":     p.AgentStatus,
			"state_change_seq": h.seq[p.ID],
		})
	}
	return map[string]any{"type": "agent_list", "agents": agents}, nil
}

func (h *Herdr) tabList(Request) (any, *ErrorBody) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return map[string]any{"type": "tab_list", "tabs": slices.Clone(h.tabs)}, nil
}

// agentPrompt accepts a prompt for a pane ID or an agent's name, the two
// targets herdr takes.
func (h *Herdr) agentPrompt(req Request) (any, *ErrorBody) {
	var params struct {
		Target string `json:"target"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Target == "" {
		return nil, &ErrorBody{Code: "invalid_request", Message: "agent.prompt needs a target"}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	i := slices.IndexFunc(h.panes, func(p Pane) bool {
		return p.Agent != "" && (p.ID == params.Target || (p.AgentName != "" && p.AgentName == params.Target))
	})
	if i < 0 {
		return nil, notFound(params.Target)
	}
	h.prompts = append(h.prompts, Prompt{Target: params.Target, Text: params.Text})
	return map[string]any{"type": "agent_prompted", "agent": map[string]any{"pane_id": h.panes[i].ID}}, nil
}

func notFound(paneID string) *ErrorBody {
	return &ErrorBody{Code: "pane_not_found", Message: "pane " + paneID + " not found"}
}
