package engine_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/engine/enginetest"
)

// flood prints n different errors from web, as a dev server might for every
// request, in batches small enough that the window never skips one.
func flood(a *enginetest.Adapter, n int, prefix string) {
	const batch = 32 // the window's limit on captures in progress
	lines := make([]string, 0, batch)
	for i := range n {
		lines = append(lines, fmt.Sprintf("Error: %s %d", prefix, i))
		if len(lines) == batch || i == n-1 {
			a.Print(web, lines...)
			a.Advance(time.Second) // options() has a one second capture wait
			lines = lines[:0]
		}
	}
}

func cappedAdapter(maxEntries int) *enginetest.Adapter {
	opts := options()
	opts.MaxEntries = maxEntries
	// Long enough that a settle period started before a flood is still running
	// after it.
	opts.Settle = 24 * time.Hour
	a := enginetest.New(opts)
	a.Watch(web, "web", "next dev")
	return a
}

func TestFloodStaysAtTheCap(t *testing.T) {
	// Most of a flood's time goes on reading the lines, not on the cap, so only
	// the default gets the full 20,000.
	tests := []struct {
		name       string
		maxEntries int
		n          int
		want       int
	}{
		{"the default", 0, 20000, 200},
		{"the smallest cap", 10, 2000, 10},
		{"a bigger cap", 500, 2000, 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := tt.n
			a := cappedAdapter(tt.maxEntries)
			flood(a, n, "flood")
			entries := a.Engine.Entries()
			if len(entries) != tt.want {
				t.Fatalf("%d entries, want %d", len(entries), tt.want)
			}
			if got := a.Engine.Dropped(); got != n-tt.want {
				t.Fatalf("dropped %d, want %d", got, n-tt.want)
			}
			// The newest are kept, newest first.
			if got, want := entries[0].Lines[0], fmt.Sprintf("Error: flood %d", n-1); got != want {
				t.Fatalf("newest entry %q, want %q", got, want)
			}
			if got, want := entries[len(entries)-1].Lines[0], fmt.Sprintf("Error: flood %d", n-tt.want); got != want {
				t.Fatalf("oldest entry %q, want %q", got, want)
			}
		})
	}
}

func TestCheckedEntriesSurviveAFlood(t *testing.T) {
	const boom = "Error: boom"
	tests := []struct {
		name string
		run  func(a *enginetest.Adapter, id engine.EntryID)
		want engine.State
	}{
		{
			name: "sent and waiting on the agent",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
			},
			want: engine.StateSent,
		},
		{
			name: "sent and settling",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
			},
			want: engine.StateSent,
		},
		{
			name: "still failing",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Print(web, boom)
				a.Advance(2 * time.Second)
			},
			want: engine.StateStillFailing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := cappedAdapter(10)
			a.Print(web, boom)
			a.Advance(2 * time.Second)
			id := only(t, a).ID
			tt.run(a, id)

			flood(a, 2000, "flood")

			entries := a.Engine.Entries()
			if len(entries) != 10 {
				t.Fatalf("%d entries, want 10", len(entries))
			}
			for _, en := range entries {
				if en.ID == id {
					if en.State != tt.want {
						t.Fatalf("state %v, want %v", en.State, tt.want)
					}
					if _, err := a.Engine.Message(id); err != nil {
						t.Fatalf("the checked entry can't be sent: %v", err)
					}
					return
				}
			}
			t.Fatalf("the checked entry was dropped")
		})
	}
}

// When everything is being checked, the feed goes over the cap rather than drop
// one, and a new error still shows up.
func TestOverTheCapWhenEverythingIsChecked(t *testing.T) {
	a := cappedAdapter(10)
	flood(a, 10, "sent")
	for _, en := range a.Engine.Entries() {
		a.Send(en.ID, "claude")
	}
	flood(a, 3, "later")

	entries := a.Engine.Entries()
	if len(entries) != 11 {
		t.Fatalf("%d entries, want the 10 sent plus the newest", len(entries))
	}
	if got, want := entries[0].Lines[0], "Error: later 2"; got != want {
		t.Fatalf("newest entry %q, want %q", got, want)
	}
	if got := a.Engine.Dropped(); got != 2 {
		t.Fatalf("dropped %d, want 2", got)
	}
}

func TestDroppedErrorComesBackAsANewEntry(t *testing.T) {
	a := cappedAdapter(10)
	a.Print(web, "Error: first")
	a.Advance(2 * time.Second)
	first := only(t, a)

	flood(a, 10, "flood")
	for _, en := range a.Engine.Entries() {
		if en.ID == first.ID {
			t.Fatalf("the oldest entry wasn't dropped")
		}
	}

	a.Print(web, "Error: first")
	a.Advance(2 * time.Second)
	again := a.Engine.Entries()[0]
	if again.Lines[0] != "Error: first" || again.ID == first.ID || again.Count != 1 {
		t.Fatalf("got %+v, want a fresh entry with a count of 1", again)
	}
	if !again.FirstSeen.After(first.FirstSeen) {
		t.Fatalf("FirstSeen %v, want after %v", again.FirstSeen, first.FirstSeen)
	}
}

func BenchmarkFlood(b *testing.B) {
	opts := options()
	a := enginetest.New(opts)
	a.Watch(web, "web", "next dev")
	flood(a, 200, "warm") // start at the cap, so every new error drops one
	b.ResetTimer()
	flood(a, b.N, "flood")
}
