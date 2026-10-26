package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/engine/enginetest"
)

const (
	boom                      = "Error: boom"
	recompile                 = "compiled successfully in 80 ms"
	api       engine.SourceID = "pane-2"
)

// These use options(): Settle 30s, CaptureWait 1s. So evidence counts up to 15s
// after the agent finishes and the verdict comes at 31s.
func TestFixCheck(t *testing.T) {
	tests := []struct {
		name  string
		setup []string // printed before the error is sent, boom if empty
		run   func(a *enginetest.Adapter, id engine.EntryID)
		want  engine.State
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
			name: "same error after a recompile is still failing",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(10 * time.Second)
				a.Print(web, boom)
				a.Advance(time.Minute)
			},
			want: engine.StateStillFailing,
		},
		{
			// The repeat matches at 29.5s but isn't filed until 30.5s. The verdict
			// must wait for it rather than give one at 30s and then start a
			// second entry.
			name: "same error just before settle ends is still failing, not a new entry",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(28500 * time.Millisecond)
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
			name: "a recompile after the agent finished is fixed",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(2 * time.Second)
				a.Print(web, "GET /health 200", recompile, "GET /health 200")
				a.Advance(29 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			// Vite and jest --watch clear the screen before each run. That
			// draws over earlier lines but swallows nothing, so it mustn't
			// stop a fix being called fixed.
			name: "a recompile after a cleared screen is fixed",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(2 * time.Second)
				a.Print(web, "\x1b[2J\x1b[3J\x1b[H"+recompile)
				a.Advance(29 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			// Anyone who can reach the server can choose a request's path.
			name: "a recompile in a request path isn't evidence",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(2 * time.Second)
				a.Print(web, "GET /compiled/successfully 404 in 3ms", `"GET /x?ready in 3ms HTTP/1.1" 404`,
					"Not Found: /compiled/successfully", `No route matches [GET] "/x&ready in 3ms"`)
				a.Advance(29 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name: "no verdict before the capture wait after settle",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(2 * time.Second)
				a.Print(web, recompile)
				a.Advance(28500 * time.Millisecond)
			},
			want: engine.StateSent,
		},
		{
			name: "a silent settle period is unverified",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(31 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name: "only request logs is unverified",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				for range 10 {
					a.Advance(time.Second)
					a.Print(web, "GET /notices 200 in 12ms", "GET /health 200 in 1ms")
				}
				a.Advance(21 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			// It may have been printed before the agent's last edit.
			name: "a recompile at the moment the agent finishes isn't evidence",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Print(web, recompile)
				a.Advance(31 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name: "a recompile while the agent works isn't evidence",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(time.Second)
				a.Finish("claude")
				a.Advance(31 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name: "a recompile exactly half way through counts",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(15 * time.Second)
				a.Print(web, recompile)
				a.Advance(16 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			// The error hasn't had long enough to come back.
			name: "a recompile late in the settle period isn't evidence",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(15*time.Second + time.Millisecond)
				a.Print(web, recompile)
				a.Advance(16 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name: "a recompile in another pane isn't evidence",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(api, recompile)
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name: "a gap during settle is unverified",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Run([]enginetest.Step{{Wait: 5 * time.Second, Source: web, Gap: true}})
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name: "an unsure read during settle is unverified",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Run([]enginetest.Step{{Wait: time.Second, Source: web, Unsure: true, Lines: []string{recompile}}})
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			// Output from before the verdict may have been missed.
			name: "a gap after settle but before the verdict is unverified",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Run([]enginetest.Step{{Wait: 29500 * time.Millisecond, Source: web, Gap: true}})
				a.Advance(time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name: "a gap while the agent works doesn't count against the settle period",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Run([]enginetest.Step{{Wait: time.Second, Source: web, Gap: true}})
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(30 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			name: "a gap in another pane doesn't count",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Run([]enginetest.Step{{Wait: time.Second, Source: api, Gap: true, Unsure: true}})
				a.Advance(30 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			// A Gap read's lines may be the old screen printed again.
			name: "the same error in a gap read is unverified, not still failing",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Run([]enginetest.Step{{Wait: 5 * time.Second, Source: web, Gap: true, Lines: []string{boom}}})
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			// The adapter reads often. Only the read the repeat came from matters.
			name: "the same error in a gap read stays unverified through later gaps",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Run([]enginetest.Step{
					{Wait: 5 * time.Second, Source: web, Gap: true, Lines: []string{boom}},
					{Wait: 300 * time.Millisecond, Source: web, Gap: true},
					{Wait: 300 * time.Millisecond, Source: web, Gap: true},
				})
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name: "the same error in an unsure read is still failing",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Run([]enginetest.Step{{Wait: 5 * time.Second, Source: web, Unsure: true, Lines: []string{boom}}})
				a.Advance(30 * time.Second)
			},
			want: engine.StateStillFailing,
		},
		{
			name:  "a request error isn't fixed by a recompile",
			setup: []string{"GET /notices 500 in 30ms", boom},
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name:  "a request error is fixed by the same request again",
			setup: []string{`Started GET "/notices" for 127.0.0.1`, boom},
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, `Started GET "/notices" for 127.0.0.1`, "Completed 200 OK in 12ms")
				a.Advance(30 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			// Next.js and Django log the request after the error.
			name:  "a request logged after the error is the trigger",
			setup: []string{boom, "GET /notices 500 in 30ms"},
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, "GET /notices 200 in 12ms")
				a.Advance(30 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			name:  "a request error isn't fixed by a different request",
			setup: []string{"GET /notices 500 in 30ms", boom},
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, "GET /health 200 in 1ms", "POST /notices 200 in 3ms")
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			// Rails logs the 500 before the exception, and the 500 is what
			// matches.
			name:  "a request that ended with a 5xx is the trigger",
			setup: []string{`Started GET "/notices" for 127.0.0.1`, "Completed 500 Internal Server Error in 5ms"},
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name:  "a request that ended with a 5xx is fixed by it ending well",
			setup: []string{`Started GET "/notices" for 127.0.0.1`, "Completed 500 Internal Server Error in 5ms"},
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, `Started GET "/notices" for 127.0.0.1`, "Completed 200 OK in 3ms")
				a.Advance(30 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			// A query can put a request's words in the log anywhere on a
			// line, but only a line in a request's own form is one.
			name:  "a request error isn't fixed by the request logged in a parameter",
			setup: []string{"GET /notices 500 in 30ms", boom},
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, `  Parameters: {"q"=>"GET /notices 200"}`, "Not Found: /x GET /notices 200",
					`Started GET "/x?q=GET+/notices+200" for 127.0.0.1`)
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name:  "a request error in the Apache form is fixed by the same request again",
			setup: []string{boom, `127.0.0.1 - - [01/Jan/2026:09:00:00 +0000] "GET /notices HTTP/1.1" 500 512`},
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, `127.0.0.1 - - [01/Jan/2026:09:00:05 +0000] "GET /notices HTTP/1.1" 200 2048`)
				a.Advance(30 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			name:  "the same request failing again isn't evidence",
			setup: []string{"GET /notices 500 in 30ms", boom},
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, "GET /notices 500 in 28ms")
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			// A health check near the error has nothing to do with it, so its
			// next poll mustn't count as the error's request running again.
			name:  "a healthy request near the error isn't its trigger",
			setup: []string{"GET /health 200 in 1ms", boom, "GET /health 200 in 1ms"},
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(5 * time.Second)
				a.Print(web, "GET /health 200 in 1ms")
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name:  "with only healthy requests near it, a recompile fixes the error",
			setup: []string{"GET /health 200 in 1ms", boom},
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(30 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			// A control string can swallow lines, so the error may have come back
			// out of sight.
			name: "swallowed output during settle is unverified",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(5 * time.Second)
				a.Print(web, "\x1bPhidden", boom, "\x1b\\")
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name: "swallowed output while the agent works doesn't count",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Print(web, "\x1bPhidden", "more", "\x1b\\")
				a.Advance(time.Second)
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(30 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			// The pane may have closed because the process crashed.
			name: "unwatching during settle is unverified",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(time.Second)
				a.Engine.Unwatch(web)
				a.Advance(30 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name: "gap and unsure for a source that isn't watched are ignored",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Run([]enginetest.Step{{Wait: time.Second, Source: "pane-9", Gap: true, Unsure: true}})
				a.Advance(30 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			name: "sending again after still failing starts afresh",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Print(web, boom)
				a.Advance(2 * time.Second)
				a.Send(id, "claude")
				a.Advance(time.Minute) // working again
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(30 * time.Second)
			},
			want: engine.StateFixed,
		},
		{
			name: "sending again during settle drops the evidence so far",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(time.Second)
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(31 * time.Second)
			},
			want: engine.StateUnverified,
		},
		{
			name: "dismissing during settle stops the check",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Engine.Dismiss(id)
				a.Advance(31 * time.Second)
			},
			want: engine.StateDismissed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAdapter(t)
			a.Watch(api, "api", "go run .")
			setup := tt.setup
			if len(setup) == 0 {
				setup = []string{boom}
			}
			a.Print(web, setup...)
			a.Advance(time.Second)
			tt.run(a, only(t, a).ID)
			if got := only(t, a).State; got != tt.want {
				t.Fatalf("state = %v, want %v", got, tt.want)
			}
		})
	}
}

// Each line in ran.txt is enough to call a fix fixed, and no line in quiet.txt
// is.
func TestFixCheckEvidence(t *testing.T) {
	for _, tt := range []struct {
		file string
		want engine.State
	}{
		{"ran.txt", engine.StateFixed},
		{"quiet.txt", engine.StateUnverified},
	} {
		for _, line := range fixtureLines(t, tt.file) {
			t.Run(tt.file+"/"+line, func(t *testing.T) {
				a := newAdapter(t)
				a.Print(web, boom)
				a.Advance(time.Second)
				id := only(t, a).ID
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, line)
				a.Advance(30 * time.Second)
				if got := only(t, a).State; got != tt.want {
					t.Fatalf("state = %v, want %v", got, tt.want)
				}
			})
		}
	}
}

func fixtureLines(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fixcheck", name))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// A repeat whose capture had a gap in it still counts, because its error line
// was read fresh. The entry says the context may be incomplete.
func TestFixCheckRepeatWithGapInCapture(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, boom)
	a.Advance(time.Second)
	id := only(t, a).ID
	a.Send(id, "claude")
	a.Finish("claude")
	a.Advance(5 * time.Second)
	a.Print(web, boom)
	a.Run([]enginetest.Step{{Wait: 500 * time.Millisecond, Source: web, Gap: true}})
	a.Advance(time.Minute)

	en := only(t, a)
	if en.State != engine.StateStillFailing || !en.MaybeGap || en.Count != 2 {
		t.Fatalf("entry = %v, MaybeGap %v, count %d, want still failing, true, 2", en.State, en.MaybeGap, en.Count)
	}
}

// A different error during settle is a new entry. The original gets its own
// verdict, judged only on its own error.
func TestFixCheckDifferentError(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  engine.State
	}{
		{"after a recompile the original is fixed", []string{recompile, "Error: something else"}, engine.StateFixed},
		{"with nothing ran the original is unverified", []string{"Error: something else"}, engine.StateUnverified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAdapter(t)
			a.Print(web, boom)
			a.Advance(time.Second)
			first := only(t, a).ID
			a.Send(first, "claude")
			a.Finish("claude")
			a.Advance(time.Second)
			a.Print(web, tt.lines...)
			a.Advance(31 * time.Second)

			entries := a.Engine.Entries()
			if len(entries) != 2 {
				t.Fatalf("want 2 entries, got %d: %+v", len(entries), entries)
			}
			if entries[0].State != engine.StateNew || entries[0].Lines[0] != "Error: something else" {
				t.Fatalf("new entry = %v %q, want new", entries[0].State, entries[0].Lines)
			}
			if entries[1].ID != first || entries[1].State != tt.want {
				t.Fatalf("original = %v, want %v", entries[1].State, tt.want)
			}
		})
	}
}

// Two errors sent to one agent settle together when it finishes, and each gets
// the verdict its own error earns.
func TestFixCheckTwoEntriesOneAgent(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, boom, "", "", "Error: other")
	a.Advance(time.Second)
	entries := a.Engine.Entries()
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(entries), entries)
	}
	other, first := entries[0].ID, entries[1].ID
	a.Send(first, "claude")
	a.Send(other, "claude")
	a.Finish("claude")
	a.Advance(time.Second)
	a.Print(web, recompile)
	a.Advance(5 * time.Second)
	a.Print(web, boom)
	a.Advance(time.Minute)

	got := map[engine.EntryID]engine.State{}
	for _, en := range a.Engine.Entries() {
		got[en.ID] = en.State
	}
	if len(got) != 2 || got[first] != engine.StateStillFailing || got[other] != engine.StateFixed {
		t.Fatalf("states = %v, want %s still failing and %s fixed", got, first, other)
	}
}

// The settle period runs up to but not including its end. A repeat matched at
// the last moment inside it is still failing. One matched at the end is a new
// entry, filed before the original's verdict, so the original can't be fixed.
func TestFixCheckRepeatAtTheBoundary(t *testing.T) {
	tests := []struct {
		name         string
		at           time.Duration // after the agent finished
		wantEntries  int
		wantOriginal engine.State
	}{
		{"just inside", 30*time.Second - time.Nanosecond, 1, engine.StateStillFailing},
		{"at the end", 30 * time.Second, 2, engine.StateUnverified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAdapter(t)
			a.Print(web, boom)
			a.Advance(time.Second)
			first := only(t, a).ID
			a.Send(first, "claude")
			a.Finish("claude")
			a.Advance(time.Second)
			a.Print(web, recompile)
			a.Run([]enginetest.Step{{Wait: tt.at - time.Second, Source: web, Lines: []string{boom}}})
			a.Advance(time.Minute)

			entries := a.Engine.Entries()
			if len(entries) != tt.wantEntries {
				t.Fatalf("want %d entries, got %d: %+v", tt.wantEntries, len(entries), entries)
			}
			original := entries[len(entries)-1]
			if original.ID != first || original.State != tt.wantOriginal {
				t.Fatalf("original = %v, want %v", original.State, tt.wantOriginal)
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

// An agent that goes away before finishing ends the wait for what was sent to
// it. Once it has finished, the settle period carries on without it.
func TestFixCheckAgentGone(t *testing.T) {
	tests := []struct {
		name     string
		run      func(a *enginetest.Adapter, id engine.EntryID)
		want     engine.State
		wantNote string
	}{
		{
			name: "gone while waiting is unverified with a note",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Advance(time.Second)
				a.Engine.AgentGone("claude", a.Now())
			},
			want:     engine.StateUnverified,
			wantNote: engine.NoteAgentGone,
		},
		{
			name: "gone while waiting stays unverified after a recompile",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Engine.AgentGone("claude", a.Now())
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(time.Minute)
			},
			want:     engine.StateUnverified,
			wantNote: engine.NoteAgentGone,
		},
		{
			name: "a finish after gone changes nothing",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Engine.AgentGone("claude", a.Now())
				a.Finish("claude")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(time.Minute)
			},
			want:     engine.StateUnverified,
			wantNote: engine.NoteAgentGone,
		},
		{
			name: "gone during settle keeps the fixed verdict",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Advance(time.Second)
				a.Engine.AgentGone("claude", a.Now())
				a.Print(web, recompile)
				a.Advance(time.Minute)
			},
			want: engine.StateFixed,
		},
		{
			name: "gone during settle keeps the still failing verdict",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Finish("claude")
				a.Engine.AgentGone("claude", a.Now())
				a.Print(web, boom)
				a.Advance(2 * time.Second)
			},
			want: engine.StateStillFailing,
		},
		{
			name: "another agent going leaves it waiting",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Engine.AgentGone("codex", a.Now())
				a.Advance(time.Minute)
			},
			want: engine.StateSent,
		},
		{
			name: "gone before it was sent changes nothing",
			run: func(a *enginetest.Adapter, _ engine.EntryID) {
				a.Engine.AgentGone("claude", a.Now())
			},
			want: engine.StateNew,
		},
		{
			name: "sent again after gone is checked afresh",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Engine.AgentGone("claude", a.Now())
				a.Send(id, "codex")
				a.Finish("codex")
				a.Advance(time.Second)
				a.Print(web, recompile)
				a.Advance(time.Minute)
			},
			want: engine.StateFixed,
		},
		{
			name: "sent again after gone waits on the new agent",
			run: func(a *enginetest.Adapter, id engine.EntryID) {
				a.Send(id, "claude")
				a.Engine.AgentGone("claude", a.Now())
				a.Send(id, "codex")
				a.Advance(time.Minute)
			},
			want: engine.StateSent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAdapter(t)
			a.Print(web, boom)
			a.Advance(time.Second)
			tt.run(a, only(t, a).ID)
			en := only(t, a)
			if en.State != tt.want || en.Note != tt.wantNote {
				t.Fatalf("state = %v, note = %q, want %v, %q", en.State, en.Note, tt.want, tt.wantNote)
			}
		})
	}
}

// AgentGone only touches entries sent to that agent.
func TestFixCheckAgentGoneLeavesOtherAgents(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, boom, "", "", "Error: other")
	a.Advance(time.Second)
	entries := a.Engine.Entries()
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(entries), entries)
	}
	toCodex, toClaude := entries[0].ID, entries[1].ID
	a.Send(toClaude, "claude")
	a.Send(toCodex, "codex")
	a.Engine.AgentGone("claude", a.Now())

	for _, en := range a.Engine.Entries() {
		switch en.ID {
		case toClaude:
			if en.State != engine.StateUnverified || en.Note != engine.NoteAgentGone {
				t.Fatalf("claude's entry = %v %q, want unverified with a note", en.State, en.Note)
			}
		case toCodex:
			if en.State != engine.StateSent || en.Agent != "codex" || en.Note != "" {
				t.Fatalf("codex's entry = %v %q %q, want still sent to codex", en.State, en.Agent, en.Note)
			}
		}
	}
}
