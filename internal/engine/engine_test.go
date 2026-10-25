package engine_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/engine/enginetest"
)

const web engine.SourceID = "pane-1"

func options() engine.Options {
	return engine.Options{
		Matchers:     []engine.Matcher{engine.Substring{Label: "generic", Text: "Error"}},
		Ignores:      []engine.Matcher{engine.Substring{Label: "deprecation", Text: "DeprecationWarning"}},
		ContextLines: 2,
		CaptureWait:  time.Second,
		Settle:       30 * time.Second,
		Cooldown:     time.Minute,
	}
}

func newAdapter(t *testing.T) *enginetest.Adapter {
	t.Helper()
	a := enginetest.New(options())
	a.Watch(web, "web", "next dev")
	return a
}

func only(t *testing.T, a *enginetest.Adapter) engine.Entry {
	t.Helper()
	en, err := a.Only()
	if err != nil {
		t.Fatal(err)
	}
	return en
}

// The whole loop, as the issue describes it: an error comes in, waits for the rest
// of its output, is sent, the agent finishes and the settle period gives a verdict.
func TestEndToEnd(t *testing.T) {
	a := newAdapter(t)

	a.Print(web,
		"ready on http://localhost:3000",
		"GET /notices 200",
		"TypeError: Cannot read properties of undefined (reading 'map')",
		"    at NoticeList (app/notices/page.tsx:14:22)",
	)
	if got := a.Engine.Entries(); len(got) != 0 {
		t.Fatalf("entry appeared before the capture wait: %+v", got)
	}

	a.Advance(500 * time.Millisecond)
	a.Print(web, "    at renderWithHooks (react-dom.js:1:1)", "GET /favicon.ico 200")
	a.Advance(500 * time.Millisecond)

	en := only(t, a)
	if en.State != engine.StateNew {
		t.Fatalf("state = %v, want new", en.State)
	}
	wantLines := []string{"TypeError: Cannot read properties of undefined (reading 'map')"}
	wantBefore := []string{"ready on http://localhost:3000", "GET /notices 200"}
	wantAfter := []string{"    at NoticeList (app/notices/page.tsx:14:22)", "    at renderWithHooks (react-dom.js:1:1)"}
	if !slices.Equal(en.Lines, wantLines) || !slices.Equal(en.Before, wantBefore) || !slices.Equal(en.After, wantAfter) {
		t.Fatalf("captured\n lines %q\n before %q\n after %q", en.Lines, en.Before, en.After)
	}
	if en.Source.Title != "web" || en.Matcher != "generic" || en.Count != 1 || !en.FirstSeen.Equal(enginetest.Start()) {
		t.Fatalf("entry = %+v", en)
	}

	a.Send(en.ID, "claude")
	if got := only(t, a); got.State != engine.StateSent || got.Agent != "claude" {
		t.Fatalf("after MarkSent: state %v agent %q", got.State, got.Agent)
	}

	a.Advance(time.Minute) // the agent is working. No verdict until it finishes.
	if got := only(t, a); got.State != engine.StateSent {
		t.Fatalf("verdict before the agent finished: %v", got.State)
	}

	a.Finish("claude")
	// The verdict comes a capture wait after the settle period, so a repeat that
	// matched just before the end is filed first.
	a.Advance(30 * time.Second)
	if got := only(t, a); got.State != engine.StateSent {
		t.Fatalf("verdict before the settle period and capture wait ended: %v", got.State)
	}
	a.Advance(time.Second)
	if got := only(t, a); got.State != engine.StateUnverified {
		t.Fatalf("after settle: state %v, want unverified", got.State)
	}
}

func TestEntriesReturnsACopy(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, "before", "Error: original", "after")
	a.Advance(time.Second)

	got := a.Engine.Entries()
	got[0].Lines[0] = "changed"
	got[0].Before[0] = "changed"
	got[0].After[0] = "changed"
	got[0].State = engine.StateFixed
	got[0] = engine.Entry{}

	en := only(t, a)
	if en.Lines[0] != "Error: original" || en.Before[0] != "before" || en.After[0] != "after" || en.State != engine.StateNew {
		t.Fatalf("engine changed through the copy: %+v", en)
	}
}

func TestEntriesNewestFirst(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, "Error: first")
	a.Advance(time.Second)
	a.Print(web, "Error: second")
	a.Advance(time.Second)

	entries := a.Engine.Entries()
	if len(entries) != 2 || entries[0].Lines[0] != "Error: second" || entries[0].ID == entries[1].ID {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestUnknownEntries(t *testing.T) {
	a := newAdapter(t)
	a.Send("nope", "claude")
	a.Engine.Dismiss("nope")
	if _, err := a.Engine.Message("nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("Message on an unknown id: err = %v", err)
	}
	if got := a.Engine.Entries(); len(got) != 0 {
		t.Fatalf("entries = %+v", got)
	}
}
