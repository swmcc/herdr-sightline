package engine_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/engine/enginetest"
)

// newAdapterWith is newAdapter with opts in place of options().
func newAdapterWith(opts engine.Options) *enginetest.Adapter {
	a := enginetest.New(opts)
	a.Watch(web, "web", "next dev")
	return a
}

// frames is n lines of a JavaScript stack trace, which carry on an error block.
func frames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "    at f" + string(rune('1'+i)) + " (app.js:1:1)"
	}
	return out
}

func byFirstLine(a *enginetest.Adapter) map[string]engine.Entry {
	out := map[string]engine.Entry{}
	for _, en := range a.Engine.Entries() {
		out[en.Lines[0]] = en
	}
	return out
}

func TestReconfigureKeepsEntries(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, "Error: kept")
	a.Advance(time.Second)
	before := a.Engine.Entries()

	a.Engine.Reconfigure(engine.Options{})
	if got := a.Engine.Entries(); !slices.EqualFunc(got, before, func(x, y engine.Entry) bool {
		return x.ID == y.ID && x.State == y.State && slices.Equal(x.Lines, y.Lines)
	}) {
		t.Fatalf("entries changed:\n got %+v\nwant %+v", got, before)
	}
}

func TestReconfigureMatchersApplyToTheNextLine(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, "Error: caught before")
	opts := options()
	opts.Matchers = []engine.Matcher{engine.Substring{Label: "fatal", Text: "FATAL"}}
	a.Engine.Reconfigure(opts)
	a.Print(web, "Error: not any more", "FATAL: out of memory")
	a.Advance(time.Second)

	got := byFirstLine(a)
	if len(got) != 2 {
		t.Fatalf("entries = %+v, want the capture already in progress and the new match", got)
	}
	if _, ok := got["Error: caught before"]; !ok {
		t.Error("a capture in progress was lost")
	}
	if en, ok := got["FATAL: out of memory"]; !ok || en.Matcher != "fatal" {
		t.Errorf("new matcher didn't apply: %+v", got)
	}
}

// A new ignore rule stops the next line alerting and dismisses new entries it
// matches. Anything further along is left alone.
func TestReconfigureIgnoresDismissOnlyNewEntries(t *testing.T) {
	const recompile = "compiled successfully in 80 ms"
	catch := func(a *enginetest.Adapter) engine.EntryID {
		a.Print(web, "Error: noisy")
		a.Advance(time.Second)
		return only(t, a).ID
	}
	settle := func(a *enginetest.Adapter) engine.EntryID {
		id := catch(a)
		a.Send(id, "claude")
		a.Finish("claude")
		return id
	}
	tests := []struct {
		name          string
		setup         func(a *enginetest.Adapter) engine.EntryID
		want          engine.State
		wantDismissed int
	}{
		{"new", catch, engine.StateDismissed, 1},
		{"sent", func(a *enginetest.Adapter) engine.EntryID {
			id := catch(a)
			a.Send(id, "claude")
			return id
		}, engine.StateSent, 0},
		{"settling", settle, engine.StateSent, 0},
		{"still failing", func(a *enginetest.Adapter) engine.EntryID {
			id := settle(a)
			a.Advance(time.Second)
			a.Print(web, "Error: noisy")
			a.Advance(time.Second)
			return id
		}, engine.StateStillFailing, 0},
		{"fixed", func(a *enginetest.Adapter) engine.EntryID {
			id := settle(a)
			a.Advance(time.Second)
			a.Print(web, recompile)
			a.Advance(time.Minute)
			return id
		}, engine.StateFixed, 0},
		{"unverified", func(a *enginetest.Adapter) engine.EntryID {
			id := settle(a)
			a.Advance(time.Minute)
			return id
		}, engine.StateUnverified, 0},
		{"dismissed", func(a *enginetest.Adapter) engine.EntryID {
			id := catch(a)
			a.Engine.Dismiss(id)
			return id
		}, engine.StateDismissed, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAdapter(t)
			id := tt.setup(a)
			opts := options()
			opts.Ignores = append(opts.Ignores, engine.Substring{Label: "noisy", Text: "noisy"})
			if n := a.Engine.Reconfigure(opts); n != tt.wantDismissed {
				t.Errorf("Reconfigure dismissed %d, want %d", n, tt.wantDismissed)
			}
			en := only(t, a)
			if en.ID != id || en.State != tt.want {
				t.Errorf("entry %s is %v, want %s %v", en.ID, en.State, id, tt.want)
			}
			// The next line is ignored too.
			a.Advance(2 * time.Minute)
			a.Print(web, "Error: noisy again")
			a.Advance(time.Second)
			if n := len(a.Engine.Entries()); n != 1 {
				t.Errorf("%d entries, want the new line ignored", n)
			}
		})
	}
}

func TestReconfigureClosingLineAppliesToTheNextMessage(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, "Error: boom")
	a.Advance(time.Second)
	id := only(t, a).ID

	opts := options()
	opts.ClosingLine = "Explain the cause before changing anything."
	a.Engine.Reconfigure(opts)
	msg, err := a.Engine.Message(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, opts.ClosingLine) {
		t.Errorf("message doesn't end with the new closing line:\n%s", msg)
	}
}

func TestReconfigureContextLinesApplyToNewCaptures(t *testing.T) {
	tests := []struct {
		name       string
		from, to   int
		wantAfter  int      // lines after the error caught before the change
		wantBefore []string // context before an error caught after it
		wantNext   int      // lines after that error
	}{
		{"growing", 2, 4, 2, []string{"    at f3 (app.js:1:1)", "    at f4 (app.js:1:1)", "between", "next"}, 4},
		{"shrinking", 4, 1, 4, []string{"next"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := options()
			opts.ContextLines = tt.from
			a := newAdapterWith(opts)
			a.Print(web, "one", "two", "Error: first")
			opts.ContextLines = tt.to
			a.Engine.Reconfigure(opts)
			a.Print(web, frames(4)...)
			a.Advance(time.Second)
			first := only(t, a)
			if len(first.After) != tt.wantAfter {
				t.Errorf("capture in progress kept %d lines after, want %d: %q", len(first.After), tt.wantAfter, first.After)
			}

			a.Print(web, "between", "next", "Error: second")
			a.Print(web, frames(4)...)
			a.Advance(time.Second)
			second := byFirstLine(a)["Error: second"]
			if !slices.Equal(second.Before, tt.wantBefore) {
				t.Errorf("new capture's context before = %q, want %q", second.Before, tt.wantBefore)
			}
			if len(second.After) != tt.wantNext {
				t.Errorf("new capture kept %d lines after, want %d: %q", len(second.After), tt.wantNext, second.After)
			}
		})
	}
}

// A capture in progress keeps its wait, and a verdict still waits for it, so a
// shorter CaptureWait can't let a verdict jump ahead of a repeat.
func TestReconfigureCaptureWaitAppliesToNewCaptures(t *testing.T) {
	opts := options()
	opts.CaptureWait = 5 * time.Second
	a := newAdapterWith(opts)
	a.Print(web, "Error: boom")
	a.Advance(5 * time.Second)
	id := only(t, a).ID
	a.Send(id, "claude")
	a.Finish("claude")

	a.Advance(29 * time.Second)
	a.Print(web, "Error: boom") // inside the settle period, filed 5s later
	a.Advance(time.Second)
	opts.CaptureWait = time.Second
	a.Engine.Reconfigure(opts)

	a.Advance(time.Second) // past the settle period and the new wait
	if got := only(t, a); got.State != engine.StateSent {
		t.Fatalf("state = %v, want sent until the repeat is filed", got.State)
	}
	a.Advance(3 * time.Second)
	if got := only(t, a); got.State != engine.StateStillFailing || got.Count != 2 {
		t.Fatalf("state = %v count %d, want still failing with the repeat counted", got.State, got.Count)
	}

	// A capture that starts now uses the new wait.
	a.Print(web, "Error: fresh")
	a.Advance(time.Second)
	if _, ok := byFirstLine(a)["Error: fresh"]; !ok {
		t.Error("a new capture didn't use the shorter wait")
	}
}

func TestReconfigureSettleAppliesToNewSettlePeriods(t *testing.T) {
	a := newAdapter(t) // 30s settle
	a.Print(web, "Error: first")
	a.Advance(time.Second)
	first := only(t, a).ID
	a.Send(first, "claude")
	a.Finish("claude")

	opts := options()
	opts.Settle = 2 * time.Second
	a.Engine.Reconfigure(opts)

	// Evidence ten seconds in is inside the first half of the 30s period that
	// was already under way, though not of a 2s one.
	a.Advance(10 * time.Second)
	a.Print(web, "compiled successfully in 80 ms")
	a.Advance(20 * time.Second)
	if got := byFirstLine(a)["Error: first"]; got.State != engine.StateSent {
		t.Fatalf("state = %v, want the settle period under way to keep its length", got.State)
	}
	a.Advance(time.Second)
	if got := byFirstLine(a)["Error: first"]; got.State != engine.StateFixed {
		t.Fatalf("state = %v, want fixed", got.State)
	}

	a.Print(web, "Error: second")
	a.Advance(time.Second)
	second := byFirstLine(a)["Error: second"].ID
	a.Send(second, "claude")
	a.Finish("claude")
	a.Advance(3 * time.Second) // the new 2s settle plus the capture wait
	if got := byFirstLine(a)["Error: second"]; got.State != engine.StateUnverified {
		t.Fatalf("state = %v, want a new settle period to use the new length", got.State)
	}
}

func TestReconfigureCooldownAppliesFromNow(t *testing.T) {
	a := newAdapter(t) // one minute cooldown
	a.Print(web, "Error: loop")
	a.Advance(time.Second)
	a.Engine.Dismiss(only(t, a).ID)

	opts := options()
	opts.Cooldown = 5 * time.Second
	a.Engine.Reconfigure(opts)
	a.Advance(10 * time.Second)
	a.Print(web, "Error: loop")
	a.Advance(time.Second)
	if n := len(a.Engine.Entries()); n != 2 {
		t.Fatalf("%d entries, want the repeat after the new cooldown to be fresh", n)
	}
}

func TestReconfigureMaxEntriesTrims(t *testing.T) {
	tests := []struct {
		name        string
		send        bool
		want        int
		wantDropped int
	}{
		{"new entries are dropped", false, 10, 5},
		{"entries being checked never are", true, 15, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := cappedAdapter(200)
			flood(a, 15, "flood")
			if tt.send {
				for _, en := range a.Engine.Entries() {
					a.Send(en.ID, "claude")
				}
			}
			opts := options()
			opts.MaxEntries = 10
			a.Engine.Reconfigure(opts)
			if n := len(a.Engine.Entries()); n != tt.want {
				t.Errorf("%d entries, want %d", n, tt.want)
			}
			if n := a.Engine.Dropped(); n != tt.wantDropped {
				t.Errorf("dropped %d, want %d", n, tt.wantDropped)
			}
		})
	}
}
