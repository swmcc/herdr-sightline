package engine_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/engine/enginetest"
)

// A pane that moves to another workspace gets a new id (#56).
const (
	beforeMove engine.SourceID = "w1:p3"
	afterMove  engine.SourceID = "w2:p1"
)

// movedAdapter watches beforeMove and catches boom on it, so there's one entry.
func movedAdapter(t *testing.T) (*enginetest.Adapter, engine.EntryID) {
	t.Helper()
	a := enginetest.New(options())
	a.Watch(beforeMove, "web", "next dev")
	a.Print(beforeMove, boom)
	a.Advance(time.Second)
	return a, only(t, a).ID
}

func rename(t *testing.T, a *enginetest.Adapter) {
	t.Helper()
	if !a.Engine.Rename(beforeMove, afterMove) {
		t.Fatal("Rename moved nothing")
	}
}

// These use options(): Settle 30s, CaptureWait 1s.
func TestRenameCarriesTheFixCheck(t *testing.T) {
	tests := []struct {
		name      string
		run       func(a *enginetest.Adapter, id engine.EntryID)
		want      engine.State
		wantCount int
	}{
		{
			name: "sent before the move, a recompile after it is fixed",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				rename(t, a)
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(afterMove, recompile)
				a.Advance(31 * time.Second)
			},
			want:      engine.StateFixed,
			wantCount: 1,
		},
		{
			name: "a move during settle, then the same error, is still failing",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				rename(t, a)
				a.Advance(time.Second)
				a.Print(afterMove, boom)
				a.Advance(2 * time.Second)
			},
			want:      engine.StateStillFailing,
			wantCount: 2,
		},
		{
			name: "a move during settle, then a recompile, is fixed",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				rename(t, a)
				a.Advance(time.Second)
				a.Print(afterMove, recompile)
				a.Advance(30 * time.Second)
			},
			want:      engine.StateFixed,
			wantCount: 1,
		},
		{
			name: "output under the old id after the move is ignored",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				rename(t, a)
				a.Advance(time.Second)
				a.Print(beforeMove, recompile)
				a.Advance(31 * time.Second)
			},
			want:      engine.StateUnverified,
			wantCount: 1,
		},
		{
			name: "a gap on the new id during settle is unverified",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				rename(t, a)
				a.Run([]enginetest.Step{{Source: afterMove, Gap: true, Lines: []string{recompile}}})
				a.Advance(31 * time.Second)
			},
			want:      engine.StateUnverified,
			wantCount: 1,
		},
		{
			name: "a repeat after the move adds to the same entry",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				rename(t, a)
				a.Print(afterMove, boom)
				a.Advance(2 * time.Second)
			},
			want:      engine.StateNew,
			wantCount: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, id := movedAdapter(t)
			tt.run(a, id)
			en := only(t, a)
			if en.ID != id {
				t.Fatalf("entry id = %s, want %s", en.ID, id)
			}
			if en.Source.ID != afterMove {
				t.Fatalf("source = %s, want %s", en.Source.ID, afterMove)
			}
			if en.State != tt.want {
				t.Fatalf("state = %v, want %v", en.State, tt.want)
			}
			if en.Count != tt.wantCount {
				t.Fatalf("count = %d, want %d", en.Count, tt.wantCount)
			}
		})
	}
}

// A capture in progress keeps collecting lines after the move and is filed
// under the new id, with its title and command.
func TestRenameDuringCapture(t *testing.T) {
	a := enginetest.New(options())
	a.Watch(beforeMove, "web", "next dev")
	a.Print(beforeMove, "GET /notices 500", boom)
	a.Advance(500 * time.Millisecond)
	rename(t, a)
	a.Print(afterMove, "    at handler (app.js:1:2)")
	a.Advance(time.Second)
	en := only(t, a)
	if en.Source != (engine.Source{ID: afterMove, Title: "web", Command: "next dev"}) {
		t.Fatalf("source = %+v", en.Source)
	}
	if !slices.Equal(en.Before, []string{"GET /notices 500"}) || !slices.Equal(en.After, []string{"    at handler (app.js:1:2)"}) {
		t.Fatalf("context = %q and %q, want both sides of the move", en.Before, en.After)
	}
	if en.MaybeGap {
		t.Fatal("a rename on its own flagged the capture MaybeGap")
	}
}

// The message names the pane by id, so a message built after the move names
// the new one.
func TestRenameMessageNamesTheNewPane(t *testing.T) {
	a, id := movedAdapter(t)
	msg, err := a.Engine.Message(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "pane w1:p3.") {
		t.Fatalf("before the move the message doesn't name w1:p3:\n%s", msg)
	}
	rename(t, a)
	msg, err = a.Engine.Message(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "pane w2:p1.") || strings.Contains(msg, "w1:p3") {
		t.Fatalf("after the move the message should name w2:p1 and only that:\n%s", msg)
	}
}

// Entries follow a move even when their source was unwatched just before it,
// which happens when a read finds the pane gone before the move is reported.
func TestRenameUnwatchedSource(t *testing.T) {
	a, id := movedAdapter(t)
	a.Send(id, "claude")
	a.Engine.Unwatch(beforeMove)
	rename(t, a)
	a.Watch(afterMove, "web", "next dev")
	a.Finish("claude")
	a.Advance(time.Second)
	a.Print(afterMove, recompile)
	a.Advance(31 * time.Second)
	en := only(t, a)
	if en.Source.ID != afterMove || en.State != engine.StateFixed {
		t.Fatalf("entry = %s in %v, want fixed in %s", en.Source.ID, en.State, afterMove)
	}
}

// herdr doesn't reuse ids, but if the new id is already known nothing is
// merged. The old source is unwatched instead, so its settling entry ends
// unverified rather than being judged on another pane's output.
func TestRenameToAKnownSource(t *testing.T) {
	tests := []struct {
		name string
		to   func(a *enginetest.Adapter)
	}{
		{"watched", func(a *enginetest.Adapter) { a.Watch(afterMove, "api", "go run .") }},
		{"named by an entry", func(a *enginetest.Adapter) {
			a.Watch(afterMove, "api", "go run .")
			a.Print(afterMove, "Error: other")
			a.Advance(time.Second)
			a.Engine.Unwatch(afterMove)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, id := movedAdapter(t)
			tt.to(a)
			a.Send(id, "claude")
			a.Finish("claude")
			if a.Engine.Rename(beforeMove, afterMove) {
				t.Fatal("Rename moved a source onto a known id")
			}
			a.Advance(time.Second)
			a.Print(beforeMove, recompile)
			a.Print(afterMove, recompile)
			a.Advance(31 * time.Second)
			for _, en := range a.Engine.Entries() {
				if en.ID != id {
					continue
				}
				if en.Source.ID != beforeMove || en.State != engine.StateUnverified {
					t.Fatalf("entry = %s in %v, want unverified in %s", en.Source.ID, en.State, beforeMove)
				}
				return
			}
			t.Fatalf("entry %s has gone", id)
		})
	}
}

// Renaming a source to its own id does nothing, and in particular doesn't
// unwatch it.
func TestRenameToItself(t *testing.T) {
	a, _ := movedAdapter(t)
	if a.Engine.Rename(beforeMove, beforeMove) {
		t.Fatal("Rename to the same id said it moved something")
	}
	a.Print(beforeMove, boom)
	a.Advance(2 * time.Second)
	if got := only(t, a).Count; got != 2 {
		t.Fatalf("count = %d, want 2, so the source is still watched", got)
	}
}
