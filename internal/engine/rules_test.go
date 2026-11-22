package engine_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/engine/enginetest"
)

// repoRules are a project file's rules: a pattern of its own and an ignore
// rule for one error it doesn't care about.
func repoRules() engine.Rules {
	return engine.Rules{
		Matchers: []engine.Matcher{engine.Substring{Label: "repo", Text: "FATAL"}},
		Ignores:  []engine.Matcher{engine.Substring{Label: "repo", Text: "Error: flaky"}},
	}
}

func twoPanes(t *testing.T) *enginetest.Adapter {
	t.Helper()
	a := newAdapter(t)
	a.Watch(api, "api", "go run .")
	a.Engine.SetRules(web, repoRules())
	return a
}

func TestSourceRules(t *testing.T) {
	tests := []struct {
		name       string
		source     engine.SourceID
		lines      []string
		wantFirst  []string // first line of each entry, newest first
		wantHidden int
	}{
		{"a repo pattern catches errors in its own pane", web, []string{"FATAL out of memory"}, []string{"FATAL out of memory"}, 0},
		{"and not in another repo's pane", api, []string{"FATAL out of memory"}, nil, 0},
		{"a repo ignore rule hides an error and counts it", web, []string{"Error: flaky test"}, nil, 1},
		{"but only in its own pane", api, []string{"Error: flaky test"}, []string{"Error: flaky test"}, 0},
		{"the person's own ignore rule comes first and isn't counted", web, []string{"Error: DeprecationWarning"}, nil, 0},
		{"a line no pattern matches isn't counted", web, []string{"flaky but fine"}, nil, 0},
		{"a repo ignore rule can hide a repo pattern's match", web, []string{"FATAL Error: flaky"}, nil, 1},
		{"other errors still come through", web, []string{"Error: flaky test", "Error: real"}, []string{"Error: real"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := twoPanes(t)
			a.Print(tt.source, tt.lines...)
			a.Advance(2 * time.Second)
			var got []string
			for _, en := range a.Engine.Entries() {
				got = append(got, en.Lines[0])
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.wantFirst) {
				t.Errorf("entries %q, want %q", got, tt.wantFirst)
			}
			total, recent := a.Engine.Hidden()
			if total != tt.wantHidden || len(recent) != tt.wantHidden {
				t.Errorf("hidden %d (%d kept), want %d", total, len(recent), tt.wantHidden)
			}
			for _, h := range recent {
				if h.Source != tt.source || h.Line != "Error: flaky test" && h.Line != "FATAL Error: flaky" {
					t.Errorf("hidden %+v", h)
				}
			}
		})
	}
}

// A repo's ignore rule could hide the error coming back while a fix is
// checked, so after it hides anything the check can't say fixed.
func TestSourceRuleHidingStopsAFix(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  engine.State
	}{
		{"nothing hidden is fixed", []string{recompile}, engine.StateFixed},
		{"a hidden error is unverified", []string{recompile, "Error: flaky again"}, engine.StateUnverified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAdapter(t)
			a.Print(web, boom)
			a.Advance(2 * time.Second)
			en := only(t, a)
			a.Engine.SetRules(web, repoRules())
			a.Send(en.ID, "claude")
			a.Finish("claude")
			a.Advance(2 * time.Second)
			a.Print(web, tt.lines...)
			a.Advance(31 * time.Second)
			if got := only(t, a).State; got != tt.want {
				t.Errorf("state = %v, want %v", got, tt.want)
			}
		})
	}
}

// Rules from a repo only add. They never dismiss what's already in the feed,
// and a capture already under way is filed as usual.
func TestSourceRulesNeverDismiss(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, "Error: flaky filed")
	a.Advance(2 * time.Second)
	a.Print(web, "Error: flaky capturing")
	a.Engine.SetRules(web, repoRules())
	a.Advance(2 * time.Second)
	for _, en := range a.Engine.Entries() {
		if en.State != engine.StateNew {
			t.Errorf("%q is %v, want new", en.Lines[0], en.State)
		}
	}
	if n := len(a.Engine.Entries()); n != 2 {
		t.Errorf("got %d entries, want 2", n)
	}
}

func TestSourceRulesFollowARenameAndGoWithUnwatch(t *testing.T) {
	a := newAdapter(t)
	a.Engine.SetRules(web, repoRules())
	a.Engine.Rename(web, "pane-9")
	a.Print("pane-9", "Error: flaky moved")
	a.Advance(2 * time.Second)
	total, recent := a.Engine.Hidden()
	if total != 1 || recent[0].Source != "pane-9" {
		t.Fatalf("after a rename hidden = %d %+v, want 1 under pane-9", total, recent)
	}

	a.Engine.Unwatch("pane-9")
	a.Watch("pane-9", "web", "next dev")
	a.Print("pane-9", "Error: flaky again")
	a.Advance(2 * time.Second)
	if n := len(a.Engine.Entries()); n != 1 {
		t.Errorf("watched again without rules, got %d entries, want 1", n)
	}

	// Rules for a source that isn't watched go nowhere.
	a.Engine.SetRules("pane-404", repoRules())
	a.Watch("pane-404", "x", "")
	a.Print("pane-404", "Error: flaky")
	a.Advance(2 * time.Second)
	if n := len(a.Engine.Entries()); n != 2 {
		t.Errorf("rules set before watching applied, got %d entries, want 2", n)
	}
}

func TestHiddenKeepsTheLatest(t *testing.T) {
	a := newAdapter(t)
	a.Engine.SetRules(web, repoRules())
	for i := range 60 {
		a.Print(web, fmt.Sprintf("Error: flaky %d", i))
	}
	total, recent := a.Engine.Hidden()
	if total != 60 || len(recent) != 50 {
		t.Fatalf("hidden %d with %d kept, want 60 with 50", total, len(recent))
	}
	if recent[0].Line != "Error: flaky 10" || recent[49].Line != "Error: flaky 59" {
		t.Errorf("kept %q to %q, want the latest 50 oldest first", recent[0].Line, recent[49].Line)
	}
	recent[0].Line = "changed"
	if _, again := a.Engine.Hidden(); again[0].Line == "changed" {
		t.Error("Hidden handed out the engine's own slice")
	}
}
