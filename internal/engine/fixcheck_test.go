package engine_test

import (
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/engine/enginetest"
)

// These use options(): Settle 30s, CaptureWait 1s.
func TestFixCheck(t *testing.T) {
	const boom = "Error: boom"
	tests := []struct {
		name string
		run  func(a *enginetest.Adapter, id engine.EntryID)
		want engine.State
	}{
		{
			name: "same error during settle is still failing",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Print(web, boom)
				a.Advance(2 * time.Second)
			},
			want: engine.StateStillFailing,
		},
		{
			// The repeat matches at 29.5s but isn't filed until 30.5s. The verdict
			// must wait for it rather than say unverified at 30s and then start a
			// second entry.
			name: "same error just before settle ends is still failing, not a new entry",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(29500 * time.Millisecond)
				a.Print(web, boom)
				a.Advance(500 * time.Millisecond)
				a.Advance(500 * time.Millisecond)
			},
			want: engine.StateStillFailing,
		},
		{
			name: "same error before the agent finishes is only counted",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Print(web, boom)
				a.Advance(2 * time.Second)
			},
			want: engine.StateSent,
		},
		{
			name: "another agent finishing doesn't start the settle period",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("codex")
				a.Advance(time.Hour)
			},
			want: engine.StateSent,
		},
		{
			name: "finishing again doesn't push the verdict out",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				for range 4 {
					a.Advance(10 * time.Second)
					a.Finish("claude")
				}
			},
			want: engine.StateUnverified,
		},
		{
			name: "same error after the settle period ends but before a tick isn't still failing",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(29 * time.Second)
				a.Run([]enginetest.Step{{Wait: 16 * time.Second, Source: web, Lines: []string{boom}}})
			},
			want: engine.StateUnverified,
		},
		{
			name: "quiet settle period is unverified",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Print(web, "compiled successfully")
				a.Advance(31 * time.Second)
			},
			want: engine.StateUnverified,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAdapter(t)
			a.Print(web, boom)
			a.Advance(time.Second)
			tt.run(a, only(t, a).ID)
			if got := only(t, a).State; got != tt.want {
				t.Fatalf("state = %v, want %v", got, tt.want)
			}
		})
	}
}

// A repeat that matches after the settle period has ended belongs to a new
// entry, whenever the next Tick happens to run.
func TestRepeatAfterSettleIsNewWhateverTheTicks(t *testing.T) {
	tests := []struct {
		name  string
		ticks []time.Duration
	}{
		{"tick at 31s then 31.5s", []time.Duration{500 * time.Millisecond, 500 * time.Millisecond}},
		{"tick only at 31.5s", []time.Duration{time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAdapter(t)
			a.Print(web, "Error: boom")
			a.Advance(time.Second)
			first := only(t, a).ID
			a.Send(first, "claude")
			a.Finish("claude")

			// Settle ends 30s after Finish. The repeat matches at 30.5s and is due
			// at 31.5s.
			a.Run([]enginetest.Step{{Wait: 30500 * time.Millisecond, Source: web, Lines: []string{"Error: boom"}}})
			for _, d := range tt.ticks {
				a.Advance(d)
			}

			entries := a.Engine.Entries()
			if len(entries) != 2 {
				t.Fatalf("want 2 entries, got %d: %+v", len(entries), entries)
			}
			if entries[0].State != engine.StateNew || entries[0].Count != 1 {
				t.Fatalf("new entry = %v count %d, want new count 1", entries[0].State, entries[0].Count)
			}
			if entries[1].ID != first || entries[1].State != engine.StateUnverified || entries[1].Count != 1 {
				t.Fatalf("old entry = %v count %d, want unverified count 1", entries[1].State, entries[1].Count)
			}
		})
	}
}
