package herdr

import (
	"context"
	"errors"
)

// PluginPaneOpen says which plugin pane to open and where.
type PluginPaneOpen struct {
	// PluginID and Entrypoint name the pane, as in herdr-plugin.toml.
	PluginID   string
	Entrypoint string
	// Placement overrides the manifest's ("split", "tab" and so on). Empty
	// keeps the manifest's.
	Placement string
	// TargetPane is the pane to split beside. Empty lets herdr choose.
	TargetPane string
	// Direction is "right" or "down" for a split. Empty lets herdr choose.
	Direction string
	// Focus moves focus to the new pane.
	Focus bool
}

// OpenPluginPane opens one of a plugin's panes with plugin.pane.open and
// returns the new pane.
func (c *Client) OpenPluginPane(ctx context.Context, open PluginPaneOpen) (Pane, error) {
	if open.PluginID == "" || open.Entrypoint == "" {
		return Pane{}, errors.New("herdr plugin.pane.open: plugin id and entrypoint are needed")
	}
	params := struct {
		PluginID   string  `json:"plugin_id"`
		Entrypoint string  `json:"entrypoint"`
		Placement  *string `json:"placement,omitempty"`
		TargetPane *string `json:"target_pane_id,omitempty"`
		Direction  *string `json:"direction,omitempty"`
		Focus      bool    `json:"focus"`
	}{
		PluginID:   open.PluginID,
		Entrypoint: open.Entrypoint,
		Placement:  optional(open.Placement),
		TargetPane: optional(open.TargetPane),
		Direction:  optional(open.Direction),
		Focus:      open.Focus,
	}
	var out struct {
		PluginPane struct {
			Pane Pane `json:"pane"`
		} `json:"plugin_pane"`
	}
	if err := c.call(ctx, "plugin.pane.open", params, "plugin_pane_opened", &out); err != nil {
		return Pane{}, err
	}
	if out.PluginPane.Pane.ID == "" {
		return Pane{}, errors.New("herdr plugin.pane.open: the reply has no pane id")
	}
	return out.PluginPane.Pane, nil
}

// FocusPluginPane focuses a plugin's pane with plugin.pane.focus.
func (c *Client) FocusPluginPane(ctx context.Context, paneID string) error {
	params := struct {
		PaneID string `json:"pane_id"`
	}{PaneID: paneID}
	return c.call(ctx, "plugin.pane.focus", params, "plugin_pane_focused", nil)
}

// optional leaves an empty string out of the request, so herdr uses its default.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
