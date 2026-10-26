package herdr_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/swmcc/herdr-sightline/internal/herdr"
	"github.com/swmcc/herdr-sightline/internal/herdr/herdrtest"
)

func TestOpenPluginPane(t *testing.T) {
	opened := json.RawMessage(`{"type":"plugin_pane_opened","plugin_pane":{"plugin_id":"swmcc.sightline","entrypoint":"feed",
		"pane":{"pane_id":"w1:p9","terminal_id":"term_9","workspace_id":"w1","tab_id":"w1:t1","agent_status":"unknown"}}}`)
	tests := []struct {
		name       string
		open       herdr.PluginPaneOpen
		result     json.RawMessage
		wantParams map[string]any
		want       herdr.Pane
		wantErr    string
	}{
		{
			name:   "everything given",
			open:   herdr.PluginPaneOpen{PluginID: "swmcc.sightline", Entrypoint: "feed", Placement: "split", TargetPane: "w1:p1", Direction: "right", Focus: true},
			result: opened,
			wantParams: map[string]any{"plugin_id": "swmcc.sightline", "entrypoint": "feed", "placement": "split",
				"target_pane_id": "w1:p1", "direction": "right", "focus": true},
			want: herdr.Pane{ID: "w1:p9", TerminalID: "term_9", WorkspaceID: "w1", TabID: "w1:t1", AgentStatus: herdr.AgentUnknown},
		},
		{
			name:       "empty fields are left for herdr to choose",
			open:       herdr.PluginPaneOpen{PluginID: "swmcc.sightline", Entrypoint: "feed"},
			result:     opened,
			wantParams: map[string]any{"plugin_id": "swmcc.sightline", "entrypoint": "feed", "focus": false},
			want:       herdr.Pane{ID: "w1:p9", TerminalID: "term_9", WorkspaceID: "w1", TabID: "w1:t1", AgentStatus: herdr.AgentUnknown},
		},
		{
			name:    "a reply without a pane id",
			open:    herdr.PluginPaneOpen{PluginID: "swmcc.sightline", Entrypoint: "feed"},
			result:  json.RawMessage(`{"type":"plugin_pane_opened","plugin_pane":{"pane":{}}}`),
			wantErr: "no pane id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := herdrtest.New(t)
			srv.Handle("plugin.pane.open", reply(tt.result))
			got, err := newClient(t, srv).OpenPluginPane(context.Background(), tt.open)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("OpenPluginPane: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("pane = %+v, want %+v", got, tt.want)
			}
			if params := paramsOf(t, nextRequest(t, srv)); !reflect.DeepEqual(params, tt.wantParams) {
				t.Errorf("params = %v, want %v", params, tt.wantParams)
			}
		})
	}
}

func TestOpenPluginPaneNeedsANameAndEntrypoint(t *testing.T) {
	srv := herdrtest.New(t)
	if _, err := newClient(t, srv).OpenPluginPane(context.Background(), herdr.PluginPaneOpen{PluginID: "swmcc.sightline"}); err == nil {
		t.Error("OpenPluginPane sent a request without an entrypoint")
	}
}

func TestFocusPluginPane(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("plugin.pane.focus", reply(json.RawMessage(`{"type":"plugin_pane_focused","plugin_pane":{"pane":{"pane_id":"w1:p9"}}}`)))
	if err := newClient(t, srv).FocusPluginPane(context.Background(), "w1:p9"); err != nil {
		t.Fatalf("FocusPluginPane: %v", err)
	}
	if params := paramsOf(t, nextRequest(t, srv)); !reflect.DeepEqual(params, map[string]any{"pane_id": "w1:p9"}) {
		t.Errorf("params = %v", params)
	}
}
