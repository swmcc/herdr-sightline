package feed

import (
	"strings"
	"testing"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// When a pin names an agent that isn't running, the entry falls back to the
// tab and says so (#82). The pin's agent comes from the user's config and the
// agent it goes to instead comes from herdr.
func TestEntrySaysWhenThePinnedAgentIsntRunning(t *testing.T) {
	tests := []struct {
		name  string
		pins  []config.Pin
		setup func(f *fakeHerdr)
		want  string // the entry's agent line
	}{
		{
			name: "no pin",
			want: "→ claude (tab 2)",
		},
		{
			name: "the pinned agent is running",
			pins: []config.Pin{{PaneTitle: "web", Agent: "claude"}},
			want: "→ claude (tab 2)",
		},
		{
			name: "the pinned agent isn't running",
			pins: []config.Pin{{PaneTitle: "web", Agent: "codex"}},
			want: "→ claude (tab 2) (pinned agent codex isn't running)",
		},
		{
			name: "a pin for another pane",
			pins: []config.Pin{{PaneTitle: "worker", Agent: "codex"}},
			want: "→ claude (tab 2)",
		},
		{
			name:  "the fallback is blocked",
			pins:  []config.Pin{{PaneTitle: "web", Agent: "codex"}},
			setup: func(f *fakeHerdr) { f.setStatus(agentPane, herdr.AgentBlocked) },
			want:  "? asking: every agent in this tab is waiting on a prompt (pinned agent codex isn't running)",
		},
		{
			name:  "nothing to fall back to",
			pins:  []config.Pin{{PaneTitle: "web", Agent: "codex"}},
			setup: func(f *fakeHerdr) { f.removeAgent(agentPane) },
			want:  "? asking: no agent in this tab (pinned agent codex isn't running)",
		},
		{
			name: "the pin's agent is shown plainly",
			pins: []config.Pin{{PaneTitle: "web", Agent: "co\u202edex\x1b[2J" + strings.Repeat("x", 40)}},
			// The bidi override goes, the escape is shown as text and the
			// whole is cut to 32 cells.
			want: "→ claude (tab 2) (pinned agent codex\\x1b[2J" + strings.Repeat("x", 19) + "… isn't running)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.model.width = 200 // wide enough that the line isn't cut
			if tt.setup != nil {
				tt.setup(h.herdr)
			}
			cfg := config.Default()
			cfg.Pins = tt.pins
			h.model.applyConfig(config.Update{Config: cfg})
			en := h.catchError()
			if who, _ := h.model.entryTarget(en); who != tt.want {
				t.Fatalf("agent line = %q, want %q", who, tt.want)
			}
			if !strings.Contains(h.model.View(), tt.want) {
				t.Fatalf("the feed doesn't show %q:\n%s", tt.want, h.model.View())
			}
		})
	}
}

// Picking an agent with t is the person's own choice, so the pin isn't
// mentioned any more.
func TestPickedAgentDropsThePinNote(t *testing.T) {
	h := newHarness(t)
	cfg := config.Default()
	cfg.Pins = []config.Pin{{PaneTitle: "web", Agent: "codex"}}
	h.model.applyConfig(config.Update{Config: cfg})
	en := h.catchError()
	h.press("t")
	h.press("enter")
	if who, _ := h.model.entryTarget(en); strings.Contains(who, "pinned") {
		t.Fatalf("agent line = %q, want no pin note after picking", who)
	}
}
