package herdr

import (
	"context"
	"errors"
)

// AgentStatus is what herdr thinks an agent in a pane is doing.
type AgentStatus string

// The statuses herdr 0.9.1 reports.
const (
	AgentIdle    AgentStatus = "idle"
	AgentWorking AgentStatus = "working"
	AgentBlocked AgentStatus = "blocked"
	AgentDone    AgentStatus = "done"
	AgentUnknown AgentStatus = "unknown"
)

// Pane is one pane as pane.list describes it. herdr sends null for anything it
// doesn't know, which arrives here as an empty string.
type Pane struct {
	ID string `json:"pane_id"`
	// TerminalID stays the same when the pane moves to another workspace,
	// unlike ID.
	TerminalID   string      `json:"terminal_id"`
	WorkspaceID  string      `json:"workspace_id"`
	TabID        string      `json:"tab_id"`
	Focused      bool        `json:"focused"`
	Label        string      `json:"label"`
	Title        string      `json:"terminal_title_stripped"`
	Cwd          string      `json:"cwd"`
	Agent        string      `json:"agent"`
	DisplayAgent string      `json:"display_agent"`
	AgentStatus  AgentStatus `json:"agent_status"`
}

// ListPanes returns every pane herdr knows about, in every workspace.
func (c *Client) ListPanes(ctx context.Context) ([]Pane, error) {
	var out struct {
		Panes []Pane `json:"panes"`
	}
	if err := c.call(ctx, "pane.list", struct{}{}, "pane_list", &out); err != nil {
		return nil, err
	}
	return out.Panes, nil
}

// PaneRead is the text of a pane at one moment.
//
// herdr's result also carries a revision, but on 0.9.1 it's always 0, so it's
// left out rather than tempting anyone to rely on it.
type PaneRead struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	// Text is the last lines of the pane joined with newlines. It never ends in
	// a newline, so its last line may still be being written.
	Text string `json:"text"`
	// Truncated is true when the pane has more history than was asked for.
	Truncated bool `json:"truncated"`
}

// ReadPane returns the last lines of a pane's output, including scrollback.
//
// It reads the recent_unwrapped source with ANSI escapes stripped, so a long
// line comes back whole however narrow the pane is.
func (c *Client) ReadPane(ctx context.Context, paneID string, lines int) (PaneRead, error) {
	if lines <= 0 {
		return PaneRead{}, errors.New("herdr pane.read: lines must be at least 1")
	}
	params := struct {
		PaneID    string `json:"pane_id"`
		Source    string `json:"source"`
		Lines     int    `json:"lines"`
		StripANSI bool   `json:"strip_ansi"`
	}{PaneID: paneID, Source: "recent_unwrapped", Lines: lines, StripANSI: true}

	var out struct {
		Read PaneRead `json:"read"`
	}
	if err := c.call(ctx, "pane.read", params, "pane_read", &out); err != nil {
		return PaneRead{}, err
	}
	return out.Read, nil
}

// Prompt types text into the agent at target and submits it, using herdr's
// agent.prompt. It returns once herdr has accepted the prompt and doesn't wait
// for the agent to act on it.
//
// This only makes the call. Deciding when to send, and making sure that only
// ever happens after a keypress, is the caller's job.
func (c *Client) Prompt(ctx context.Context, target, text string) error {
	params := struct {
		Target string `json:"target"`
		Text   string `json:"text"`
	}{Target: target, Text: text}
	return c.call(ctx, "agent.prompt", params, "agent_prompted", nil)
}
