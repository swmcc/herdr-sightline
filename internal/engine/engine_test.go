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
	if en.Source.Title != "web" || en.Count != 1 || !en.FirstSeen.Equal(enginetest.Start) {
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
	a.Advance(29 * time.Second)
	if got := only(t, a); got.State != engine.StateSent {
		t.Fatalf("verdict before the settle period ended: %v", got.State)
	}
	a.Advance(time.Second)
	if got := only(t, a); got.State != engine.StateUnverified {
		t.Fatalf("after settle: state %v, want unverified", got.State)
	}
}

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
			name: "quiet settle period is unverified",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Print(web, "compiled successfully")
				a.Advance(30 * time.Second)
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

func TestGrouping(t *testing.T) {
	tests := []struct {
		name      string
		script    []enginetest.Step
		dismiss   bool // dismiss the first entry after the first step
		wantCount []int
	}{
		{
			name: "a crash loop is one entry with a count",
			script: []enginetest.Step{
				{Source: web, Lines: []string{"Error: EADDRINUSE"}},
				{Wait: 5 * time.Second, Source: web, Lines: []string{"Error: EADDRINUSE"}},
				{Wait: 5 * time.Second, Source: web, Lines: []string{"Error: EADDRINUSE"}},
				{Wait: time.Second},
			},
			wantCount: []int{3},
		},
		{
			name: "a different error gets its own entry",
			script: []enginetest.Step{
				{Source: web, Lines: []string{"Error: one"}},
				{Wait: 5 * time.Second, Source: web, Lines: []string{"Error: two"}},
				{Wait: time.Second},
			},
			wantCount: []int{1, 1},
		},
		{
			name: "a dismissed error repeating inside the cooldown stays dismissed",
			script: []enginetest.Step{
				{Source: web, Lines: []string{"Error: loop"}},
				{Wait: 10 * time.Second, Source: web, Lines: []string{"Error: loop"}},
				{Wait: time.Second},
			},
			dismiss:   true,
			wantCount: []int{2},
		},
		{
			name: "a dismissed error coming back after the cooldown is new",
			script: []enginetest.Step{
				{Source: web, Lines: []string{"Error: loop"}},
				{Wait: 2 * time.Minute, Source: web, Lines: []string{"Error: loop"}},
				{Wait: time.Second},
			},
			dismiss:   true,
			wantCount: []int{1, 1},
		},
		{
			name: "ignored lines never alert",
			script: []enginetest.Step{
				{Source: web, Lines: []string{"DeprecationWarning: Error handling is changing"}},
				{Wait: time.Second},
			},
		},
		{
			name: "lines from an unwatched source are dropped",
			script: []enginetest.Step{
				{Source: "pane-2", Lines: []string{"Error: elsewhere"}},
				{Wait: time.Second},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAdapter(t)
			a.Run(tt.script[:1])
			a.Advance(time.Second)
			if tt.dismiss {
				a.Engine.Dismiss(only(t, a).ID)
			}
			a.Run(tt.script[1:])
			var counts []int
			for _, en := range a.Engine.Entries() {
				counts = append(counts, en.Count)
			}
			if !slices.Equal(counts, tt.wantCount) {
				t.Fatalf("counts newest first = %v, want %v", counts, tt.wantCount)
			}
		})
	}
}

func TestGapFlagsEntriesBeingCaptured(t *testing.T) {
	tests := []struct {
		name string
		gap  enginetest.Step
		want bool
	}{
		{"gap during capture", enginetest.Step{Wait: 500 * time.Millisecond, Source: web, Gap: true}, true},
		{"gap on another source", enginetest.Step{Wait: 500 * time.Millisecond, Source: "pane-2", Gap: true}, false},
		{"gap after capture", enginetest.Step{Wait: 2 * time.Second, Source: web, Gap: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAdapter(t)
			a.Watch("pane-2", "tests", "go test")
			a.Run([]enginetest.Step{
				{Source: web, Lines: []string{"Error: lost"}},
				tt.gap,
				{Wait: 2 * time.Second},
			})
			if got := only(t, a).MaybeGap; got != tt.want {
				t.Fatalf("MaybeGap = %v, want %v", got, tt.want)
			}
		})
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

func TestUnwatchDropsCaptures(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, "Error: half way")
	a.Engine.Unwatch(web)
	a.Advance(time.Minute)
	if got := a.Engine.Entries(); len(got) != 0 {
		t.Fatalf("entries = %+v", got)
	}
}
