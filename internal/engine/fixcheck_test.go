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
