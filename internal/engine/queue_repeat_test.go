package engine_test

import (
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
)

// An error that comes back after the settle period, or after its verdict, is a
// new entry. It mustn't go on its own again: printing the same error late
// would otherwise send it over and over without ever halting the pane. A
// different error from the same pane still goes, so the hold is only for the
// repeat.
func TestQueueHoldsAnErrorThatAlreadyWentOnItsOwn(t *testing.T) {
	tests := []struct {
		name  string
		after time.Duration // after the agent finishes
		again string
		want  int // automatic sends in an hour
	}{
		{"same error after the settle period", 31 * time.Second, "Error: boom", 1},
		{"same error after the verdict", 2 * time.Minute, "Error: boom", 1},
		{"a different error", 2 * time.Minute, "Error: something else entirely", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := queueAdapter(t)
			a.Engine.SetQueueMode(qpane, true)
			catch(a, qpane, "Error: boom")
			sends := 0
			start := a.Now()
			for a.Now().Sub(start) < time.Hour {
				if cands := a.Engine.QueueCandidates(qpane); len(cands) > 0 {
					text, err := a.Engine.AutoSend(cands, qagent, a.Now())
					if err == nil {
						a.Engine.AutoDelivered(cands, qagent, text, a.Now())
						sends++
						a.Advance(5 * time.Second)
						a.Finish(qagent)
						a.Advance(tt.after)
						if sends == 1 {
							catch(a, qpane, tt.again)
						} else {
							catch(a, qpane, "Error: boom")
						}
						continue
					}
				}
				a.Advance(10 * time.Second)
			}
			if sends != tt.want {
				t.Errorf("went on its own %d times in an hour, want %d", sends, tt.want)
			}
			var held bool
			for _, en := range a.Engine.Entries() {
				if en.Hold == engine.HoldRepeat {
					held = true
				}
			}
			if !held {
				t.Error("no entry says it's held as a repeat")
			}
		})
	}
}

// A repo's project file chose its own patterns, and text a request can put in a
// log line might match one. So an error only a project pattern caught waits for
// a keypress in queue mode. An error a built-in pattern catches still goes, even
// with the project's rules in place.
func TestQueueHoldsAnErrorOnlyAProjectPatternCaught(t *testing.T) {
	rules := engine.Rules{Matchers: []engine.Matcher{engine.Substring{Label: "repo", Text: "kaboom in the widget"}}}
	tests := []struct {
		name     string
		line     string
		wantHold engine.Hold
		wantGoes bool
	}{
		{"only the project pattern", "kaboom in the widget factory", engine.HoldOwnRule, false},
		{"a built-in pattern", "Error: boom", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := queueAdapter(t)
			a.Engine.SetRules(qpane, rules, a.Now())
			a.Engine.SetQueueMode(qpane, true)
			catch(a, qpane, tt.line)
			ens := a.Engine.Entries()
			if len(ens) != 1 {
				t.Fatalf("got %d entries, want 1", len(ens))
			}
			if ens[0].Hold != tt.wantHold {
				t.Errorf("hold = %q, want %q", ens[0].Hold, tt.wantHold)
			}
			if goes := len(a.Engine.QueueCandidates(qpane)) > 0; goes != tt.wantGoes {
				t.Errorf("goes on its own = %v, want %v", goes, tt.wantGoes)
			}
		})
	}
}
