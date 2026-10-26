package injection

import (
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// Fixed must stay out of an attacker's reach as much as the message format is.
// An attacker who can print into the pane can print a line that looks like a
// recompile, but that alone mustn't turn an error that may still be there into
// "fixed".

const (
	realError = "TypeError: Cannot read properties of undefined (reading 'map')"
	recompile = "compiled successfully in 80 ms"
)

// settleWith catches realError, sends it, has the agent finish and then lets
// attack print during the settle period. It returns the entry's state once the
// verdict is in.
func settleWith(t *testing.T, attack func(e *engine.Engine, now time.Time)) engine.State {
	t.Helper()
	return settleAfter(t, nil, attack)
}

// settleAfter is settleWith with setup printed just before the error, such as
// the request it came from.
func settleAfter(t *testing.T, setup []string, attack func(e *engine.Engine, now time.Time)) engine.State {
	t.Helper()
	e := engine.New(options(t))
	e.Watch(engine.Source{ID: paneID})
	now := start
	e.Feed(paneID, append(append([]string(nil), setup...), realError), now)
	now = now.Add(2 * time.Second)
	e.Tick(now)
	entries := e.Entries()
	if len(entries) != 1 {
		t.Fatalf("want the error caught once, got %+v", entries)
	}
	id := entries[0].ID
	e.MarkSent(id, "claude", now)
	now = now.Add(5 * time.Second)
	e.AgentFinished("claude", now)
	attack(e, now.Add(time.Second))
	e.Tick(now.Add(2 * time.Minute))
	for _, en := range e.Entries() {
		if en.ID == id {
			return en.State
		}
	}
	t.Fatalf("entry %s is gone", id)
	return 0
}

func TestAttackerCantMakeAnErrorFixed(t *testing.T) {
	// The control: an honest recompile and no error is fixed, so the cases
	// below are really being kept from it.
	if got := settleWith(t, func(e *engine.Engine, now time.Time) {
		e.Feed(paneID, []string{recompile}, now)
	}); got != engine.StateFixed {
		t.Fatalf("honest recompile gave %v, want fixed", got)
	}

	tests := []struct {
		name   string
		attack func(e *engine.Engine, now time.Time)
	}{
		{
			// A request log that opens an OSC and never closes it hides the
			// real error coming back, from the person and from Sightline. Then
			// the string closes and a fake recompile follows.
			"error hidden in a string, then a fake recompile",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{"GET /\x1b]0;", realError, "    at NoticeList (app/notices/page.tsx:14:22)", "\x07" + recompile}, now)
			},
		},
		{
			"fake recompile in a read that may have missed output",
			func(e *engine.Engine, now time.Time) {
				e.Gap(paneID, now)
				e.Feed(paneID, []string{recompile}, now)
			},
		},
		{
			"fake recompile in a read that can't say what's new",
			func(e *engine.Engine, now time.Time) {
				e.Unsure(paneID, now)
				e.Feed(paneID, []string{recompile}, now)
			},
		},
		{
			"fake recompile the person can't see, in a title",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{"\x1b]0;" + recompile + "\x07"}, now)
			},
		},
		{
			"fake recompile drawn over",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{recompile + "\r\x1b[2KGET /health 200"}, now)
			},
		},
		{
			// Anyone who can reach the dev server can put words in its request
			// log.
			"fake recompile in a request path",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{"GET /compiled/successfully 404 in 3ms"}, now)
			},
		},
		{
			"fake build in a request path",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{`127.0.0.1 - - "GET /x?built%20in%201s&ready in 3ms HTTP/1.1" 404 0`}, now)
			},
		},
		{
			// Django logs the path on a line of its own before the request.
			"fake recompile in Django's not found line",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{"Not Found: /compiled/successfully", "Not Found: /x&ready in 3ms"}, now)
			},
		},
		{
			"fake recompile in a Rails routing error",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{`ActionController::RoutingError (No route matches [GET] "/compiled/successfully"):`}, now)
			},
		},
		{
			// Rails and Phoenix log every request's query, and Django decodes
			// the path, spaces and all.
			"fake recompile in logged parameters and decoded paths",
			func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{
					`  Parameters: {"q"=>"compiled successfully"}`,
					`  Parameters: %{"q" => "ready in 3ms"}`,
					"Not Found: /x ready in 3ms",
					"user-agent: compiled successfully",
					"ActiveRecord::RecordNotFound (Couldn't find User with 'id'=x compiled successfully):",
					"Searching for compiled successfully",
					"WARNING: Not Found ready in 3ms",
					"[info] Sent 404 in 3ms; q=a built in 1s",
				}, now)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := settleWith(t, tt.attack); got != engine.StateUnverified {
				t.Fatalf("state = %v, want unverified", got)
			}
		})
	}
}

// The forgeries from the injection review of #64 (#69). Each line is printed on
// its own in the settle period and none may make the error fixed.
func TestForgedSignalsArentEvidence(t *testing.T) {
	for _, line := range []string{
		// Rails with config.log_tags = [:request_id] keeps the client's
		// X-Request-Id, and host or subdomain tags do the same.
		"[compiled-successfully] Processing by NoticesController#index as HTML",
		"[compiled-successfully.example.com] Processing by NoticesController#index as HTML",
		// uvicorn with proxy headers takes the client from X-Forwarded-For.
		`INFO:     compiled-successfully:0 - "PURGE /x HTTP/1.1" 405 Method Not Allowed`,
		// nginx with a log format that starts with $host.
		`compiled-successfully 127.0.0.1 - - [01/Jan/2026:09:00:00 +0000] "PURGE / HTTP/1.1" 404 153`,
		// morgan with trust proxy.
		`compiled-successfully - - [01/Jan/2026:09:00:00 +0000] "PROPFIND / HTTP/1.1" 404 -`,
		`compiled-successfully "SEARCH / HTTP/1.1" 404 0`,
		// Python's default logging format, with a file name someone chose.
		"INFO:root:evil.py compiled successfully",
		"INFO:root:evil.py changed, reloading.",
		// Words in another script aren't a symbol.
		"Пользователь compiled successfully",
	} {
		t.Run(line, func(t *testing.T) {
			if got := settleWith(t, func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, []string{line}, now)
			}); got != engine.StateUnverified {
				t.Fatalf("state = %v, want unverified", got)
			}
		})
	}
}

// An error that came from a request is only fixed by that request again, in a
// request line's own form. A query puts a request's words anywhere it likes.
func TestForgedRequestsArentEvidence(t *testing.T) {
	setup := []string{"GET /notices 500 in 30ms"}
	// The control: the request again, succeeding, is fixed.
	if got := settleAfter(t, setup, func(e *engine.Engine, now time.Time) {
		e.Feed(paneID, []string{"GET /notices 200 in 12ms"}, now)
	}); got != engine.StateFixed {
		t.Fatalf("the request succeeding gave %v, want fixed", got)
	}
	for _, lines := range [][]string{
		{`  Parameters: {"q"=>"GET /notices 200"}`},
		{"Not Found: /x GET /notices 200"},
		{`Started GET "/x?q=GET+/notices+200" for 127.0.0.1`},
		{`127.0.0.1 - - [01/Jan/2026:09:00:00 +0000] "GET /x?q=%22GET%20/notices%20HTTP/1.1%22%20200 HTTP/1.1" 404 0`},
		{"Пользователь GET /notices 200"},
		// From the injection review of this fix: a word more in the query,
		// a bad request line Werkzeug or Django logs raw, and a user or a
		// forwarded address with spaces and quotes in it.
		{`  Parameters: {"q"=>"x GET /notices HTTP/1.1 200"}`},
		{"Search query: GET /notices HTTP/1.1 200"},
		{"::1 x GET /notices HTTP/1.1 200 y GET /x HTTP/1.1 401 - - 1.2 ms"},
		{`127.0.0.1 - - [01/Jan/2026 09:00:00] "GET /notices HTTP/1.1" 200" 400 -`},
		{`[01/Jan/2026 09:00:00] "GET /notices HTTP/1.1" 200" 400 0`},
		{`127.0.0.1 - x [d] "GET /notices HTTP/1.1" 200 y [01/Jan/2026:09:00:00 +0000] "GET /x HTTP/1.1" 401 0 "-" "ua"`},
		{`INFO:     1.2.3.4 - "GET /notices HTTP/1.1" 200 x:0 - "GET /x HTTP/1.1" 404 Not Found`},
		// Real requests anyone can send that never reach the bug, or whose
		// outcome isn't shown.
		{`Started GET "/notices?x[]=1" for 1.2.3.4`},
		{`Started GET "/notices?x[]=1" for 1.2.3.4`, "Completed 400 Bad Request in 1ms"},
		{"[info] GET /notices"},
		{"GET /notices 302 in 3ms"},
		{"GET /notices?page=zzz 404 in 3ms"},
		// Another request starting in between owns the next ending.
		{`Started GET "/notices" for 1.2.3.4`, `Started GET "/" for 1.2.3.4`, "Completed 200 OK in 2ms"},
	} {
		t.Run(lines[0], func(t *testing.T) {
			if got := settleAfter(t, setup, func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, lines, now)
			}); got != engine.StateUnverified {
				t.Fatalf("state = %v, want unverified", got)
			}
		})
	}
	// The forms with no status are fixed once the request ends well.
	for _, lines := range [][]string{
		{`Started GET "/notices" for 127.0.0.1`, "Completed 200 OK in 12ms"},
		{"[info] GET /notices", "[info] Sent 200 in 3ms"},
	} {
		t.Run(lines[0]+" then "+lines[1], func(t *testing.T) {
			if got := settleAfter(t, setup, func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, lines, now)
			}); got != engine.StateFixed {
				t.Fatalf("state = %v, want fixed", got)
			}
		})
	}
}

// Someone who can reach the server can put a request of their own next to an
// error that came from no request, so it reads as the trigger, then send it
// again to pass. And a request that only fails for one query isn't passed by
// another. From the second injection review of #69.
func TestChosenTriggersArentEvidence(t *testing.T) {
	tests := []struct {
		name          string
		setup, attack []string
		want          engine.State
	}{
		{
			name:   "a 4xx of their own isn't the trigger",
			setup:  []string{"GET /search?q=x 400 in 3ms"},
			attack: []string{"GET /search?q=x 200 in 3ms"},
			want:   engine.StateUnverified,
		},
		{
			name:   "a 4xx of their own in the common log format",
			setup:  []string{`127.0.0.1 - - [01/Jan/2026:09:00:00 +0000] "GET /search HTTP/1.1" 404 0`},
			attack: []string{`127.0.0.1 - - [01/Jan/2026:09:00:00 +0000] "GET /search HTTP/1.1" 200 5`},
			want:   engine.StateUnverified,
		},
		{
			name:   "a request of their own that ended well isn't the trigger",
			setup:  []string{`Started GET "/search" for 1.2.3.4`, "Completed 200 OK in 3ms"},
			attack: []string{`Started GET "/search" for 1.2.3.4`, "Completed 200 OK in 3ms"},
			want:   engine.StateUnverified,
		},
		{
			name:   "two requests starting, so no telling which",
			setup:  []string{`Started GET "/notices" for 127.0.0.1`, `Started GET "/search" for 1.2.3.4`},
			attack: []string{`Started GET "/search" for 1.2.3.4`, "Completed 200 OK in 3ms"},
			want:   engine.StateUnverified,
		},
		{
			name:   "two requests starting, and the real one passing",
			setup:  []string{`Started GET "/notices" for 127.0.0.1`, `Started GET "/search" for 1.2.3.4`},
			attack: []string{`Started GET "/notices" for 127.0.0.1`, "Completed 200 OK in 3ms"},
			want:   engine.StateUnverified,
		},
		{
			name:   "the same path with another query",
			setup:  []string{"GET /notices?page=abc 500 in 3ms"},
			attack: []string{"GET /notices?page=1 200 in 3ms"},
			want:   engine.StateUnverified,
		},
		// From the third review: a 505 Python's http.server gives before the
		// app runs, and an ending tied to the wrong request when requests run
		// side by side.
		{
			name:   "the server's own 505 isn't the trigger",
			setup:  []string{`127.0.0.1 - - [01/Jan/2026 09:00:00] "GET /search HTTP/2.0" 505 -`},
			attack: []string{`127.0.0.1 - - [01/Jan/2026 09:00:00] "GET /search HTTP/1.1" 200 5`},
			want:   engine.StateUnverified,
		},
		{
			name:   "the server's own 505 in Django's form",
			setup:  []string{`[01/Jan/2026 09:00:00] "GET /search HTTP/2.0" 505 0`},
			attack: []string{`[01/Jan/2026 09:00:00] "GET /search HTTP/1.1" 200 5`},
			want:   engine.StateUnverified,
		},
		{
			name:   "the server's own 501 isn't the trigger",
			setup:  []string{`127.0.0.1 - - [01/Jan/2026 09:00:00] "GET /search HTTP/1.1" 501 -`},
			attack: []string{`127.0.0.1 - - [01/Jan/2026 09:00:00] "GET /search HTTP/1.1" 200 5`},
			want:   engine.StateUnverified,
		},
		{
			name:   "a 505 for a version that is read isn't the trigger either",
			setup:  []string{`127.0.0.1 - - [01/Jan/2026 09:00:00] "GET /search HTTP/1.1" 505 -`},
			attack: []string{`127.0.0.1 - - [01/Jan/2026 09:00:00] "GET /search HTTP/1.1" 200 5`},
			want:   engine.StateUnverified,
		},
		// An ending tied to the wrong request is TestStrayEndingMakesTheTriggerUnclear.
		// The controls.
		{
			name:   "one request starting whose end isn't shown is the trigger",
			setup:  []string{"[info] GET /notices"},
			attack: []string{"[info] GET /notices", "[info] Sent 200 in 3ms"},
			want:   engine.StateFixed,
		},
		{
			name:   "with their request ended well, a recompile still fixes",
			setup:  []string{`Started GET "/search" for 1.2.3.4`, "Completed 200 OK in 3ms"},
			attack: []string{recompile},
			want:   engine.StateFixed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := settleAfter(t, tt.setup, func(e *engine.Engine, now time.Time) {
				e.Feed(paneID, tt.attack, now)
			}); got != tt.want {
				t.Fatalf("state = %v, want %v", got, tt.want)
			}
		})
	}
}

// With requests handled side by side (Rails under Puma's threads), someone
// else's fast request can end first and take the real 500 off /notices,
// leaving their own slow /y as the only request that might have failed. An
// ending that can't be tied to a request makes the trigger unclear instead.
// The Completed 500 is the error here, as it is in Rails.
func TestStrayEndingMakesTheTriggerUnclear(t *testing.T) {
	for _, tt := range []struct {
		name  string
		lines []string
		want  engine.State
	}{
		{"stray ending", []string{
			`Started GET "/y" for 1.2.3.4`,
			`Started GET "/notices" for 127.0.0.1`,
			"Completed 200 OK in 1ms",
			"Completed 500 Internal Server Error in 3ms",
		}, engine.StateUnverified},
		// From the fourth review: the real 500 lands on their /y, which
		// started after someone else's ending took /notices.
		{"real ending on their request", []string{
			`Started GET "/notices" for 127.0.0.1`,
			"Completed 200 OK in 1ms",
			`Started GET "/y" for 1.2.3.4`,
			"Completed 500 Internal Server Error in 3ms",
		}, engine.StateUnverified},
		// Without the stray ending, the slow /y and /notices are both
		// candidates, which is unclear too.
		{"two candidates", []string{
			`Started GET "/y" for 1.2.3.4`,
			`Started GET "/notices" for 127.0.0.1`,
			"Completed 500 Internal Server Error in 3ms",
		}, engine.StateUnverified},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := engine.New(options(t))
			e.Watch(engine.Source{ID: paneID})
			now := start
			e.Feed(paneID, tt.lines, now)
			now = now.Add(2 * time.Second)
			e.Tick(now)
			entries := e.Entries()
			if len(entries) != 1 {
				t.Fatalf("want one entry, got %+v", entries)
			}
			id := entries[0].ID
			e.MarkSent(id, "claude", now)
			now = now.Add(5 * time.Second)
			e.AgentFinished("claude", now)
			e.Feed(paneID, []string{`Started GET "/y" for 1.2.3.4`, "Completed 200 OK in 1ms"}, now.Add(time.Second))
			e.Tick(now.Add(2 * time.Minute))
			if got := e.Entries()[len(e.Entries())-1].State; got != tt.want {
				t.Fatalf("state = %v, want %v", got, tt.want)
			}
		})
	}
}

// Through herdr, a burst bigger than the read with a fake recompile in it comes
// back as a gap, so it can't make the error fixed either.
func TestFakeRecompileInAFloodIsntFixed(t *testing.T) {
	pane := &fakePane{}
	w := herdr.NewPaneWatcher(pane, paneID)
	w.Lines = 50
	e := engine.New(options(t))
	e.Watch(engine.Source{ID: paneID})
	now := start
	adapterPoll(t, w, e, now)
	pane.lines = append(pane.lines, realError)
	adapterPoll(t, w, e, now)
	now = now.Add(2 * time.Second)
	adapterPoll(t, w, e, now)
	entries := e.Entries()
	if len(entries) != 1 {
		t.Fatalf("want the error caught once, got %+v", entries)
	}
	id := entries[0].ID
	e.MarkSent(id, "claude", now)
	now = now.Add(time.Second)
	e.AgentFinished("claude", now)

	now = now.Add(time.Second)
	for i := range 200 {
		pane.lines = append(pane.lines, "GET /health 200 "+string(rune('a'+i%26)))
	}
	pane.lines = append(pane.lines, recompile)
	adapterPoll(t, w, e, now)
	for range 40 {
		now = now.Add(time.Second)
		adapterPoll(t, w, e, now)
	}
	if got := e.Entries()[len(e.Entries())-1].State; got != engine.StateUnverified {
		t.Fatalf("after a fake recompile in a flood the state = %v, want unverified", got)
	}
}
