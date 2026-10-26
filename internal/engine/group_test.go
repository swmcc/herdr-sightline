package engine_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/engine/enginetest"
)

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

func TestRepeatUpdatesLastSeen(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, "Error: again")
	a.Advance(5 * time.Second)
	a.Print(web, "Error: again")
	a.Advance(time.Second)
	en := only(t, a)
	if want := enginetest.Start().Add(5 * time.Second); !en.LastSeen.Equal(want) || !en.FirstSeen.Equal(enginetest.Start()) {
		t.Fatalf("FirstSeen %v LastSeen %v, want LastSeen %v", en.FirstSeen, en.LastSeen, want)
	}
}

func TestRepeatRefreshesTheSource(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, "Error: again")
	a.Advance(time.Second)
	a.Watch(web, "web", "npm run dev")
	a.Print(web, "Error: again")
	a.Advance(time.Second)

	en := only(t, a)
	if en.Count != 2 || en.Source.Command != "npm run dev" {
		t.Fatalf("count %d source %+v, want 2 and the new command", en.Count, en.Source)
	}
	msg, err := a.Engine.Message(en.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "\n| running: npm run dev\n") {
		t.Fatalf("message still names the old command:\n%s", msg)
	}
}

// lineSet matches exactly the lines it holds, so a test decides which lines start
// an error and nothing in a stack trace starts another one by accident.
type lineSet map[string]bool

func (lineSet) Name() string             { return "test" }
func (m lineSet) Match(line string) bool { return m[line] }

// sameFailure feeds two errors, each with the lines after it, and reports how many
// entries they made, with the fingerprints for when a test fails.
func sameFailure(t *testing.T, a, b []string) (int, []string) {
	t.Helper()
	opts := options()
	opts.Matchers = []engine.Matcher{lineSet{a[0]: true, b[0]: true}}
	opts.ContextLines = 20
	ad := enginetest.New(opts)
	ad.Watch(web, "web", "dev")
	ad.Print(web, a...)
	ad.Advance(5 * time.Second)
	ad.Print(web, b...)
	ad.Advance(5 * time.Second)
	var prints []string
	for _, en := range ad.Engine.Entries() {
		prints = append(prints, en.Fingerprint)
	}
	return len(prints), prints
}

func TestSameFailureGroups(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
	}{
		{"ISO 8601 timestamps",
			[]string{"2026-01-01T09:00:00.123Z ERROR db: connection refused"},
			[]string{"2026-01-01T09:00:05.987+01:00 ERROR db: connection refused"}},
		{"Python logging timestamps",
			[]string{"2026-01-01 09:00:00,123 ERROR app: boom"},
			[]string{"2026-03-14 17:41:09,004 ERROR app: boom"}},
		{"Go log timestamps",
			[]string{"2026/01/01 09:00:00 Error: boom"},
			[]string{"2026/01/02 10:11:12 Error: boom"}},
		{"Rails logger prefix with PID",
			[]string{"E, [2026-01-01T09:00:00.123456 #12345] ERROR -- : ActiveRecord::ConnectionNotEstablished"},
			[]string{"E, [2026-01-01T09:01:30.654321 #12399] ERROR -- : ActiveRecord::ConnectionNotEstablished"}},
		{"syslog prefix and PID",
			[]string{"Jan  1 09:00:00 box node[1234]: Error: worker died"},
			[]string{"Jan 12 10:11:12 box node[5678]: Error: worker died"}},
		{"bare clock times",
			[]string{"[09:00:01] Error: build failed"},
			[]string{"[09:14:44.512] Error: build failed"}},
		{"epoch seconds and milliseconds",
			[]string{"ts=1767258000 Error: boom"},
			[]string{"ts=1767258042123 Error: boom"}},
		{"durations",
			[]string{"Error: query timed out after 1503ms"},
			[]string{"Error: query timed out after 2.1s"}},
		{"spelled out durations",
			[]string{"Error: gave up after 30 seconds"},
			[]string{"Error: gave up after 45 seconds"}},
		{"ports after a colon",
			[]string{"Error: listen EADDRINUSE: address already in use :::3000"},
			[]string{"Error: listen EADDRINUSE: address already in use :::3001"}},
		{"ports in a URL",
			[]string{"Error: fetch failed for http://localhost:3000/api"},
			[]string{"Error: fetch failed for http://localhost:4123/api"}},
		{"IP and port",
			[]string{"Error: connect ECONNREFUSED 127.0.0.1:5432"},
			[]string{"Error: connect ECONNREFUSED 10.0.3.7:5433"}},
		{"named port",
			[]string{"Error: could not bind port 8080"},
			[]string{"Error: could not bind port 8081"}},
		{"PIDs",
			[]string{"Error: worker (pid: 1234) exited"},
			[]string{"Error: worker (pid: 98765) exited"}},
		{"puma worker PIDs",
			[]string{"[41] ! Unable to load application: NameError: uninitialized constant Foo"},
			[]string{"[58] ! Unable to load application: NameError: uninitialized constant Foo"}},
		{"request ids",
			[]string{"Error: request_id=abcXYZ12 upstream failed"},
			[]string{"Error: request_id=Qq-77_zz upstream failed"}},
		{"trace ids",
			[]string{"Error: upstream failed trace 4bf92f3577b34da6a3ce929d0e0e4736"},
			[]string{"Error: upstream failed trace 00f067aa0ba902b7a3ce929d0e0e4736"}},
		{"UUIDs",
			[]string{`[1f6d0c3a-5b8e-4f8e-9a2a-0c4b1c3a2f11] ActionController::RoutingError (No route matches [GET] "/x")`},
			[]string{`[9b2e7d41-0a1c-4c55-8d3e-77aa1b2c3d4e] ActionController::RoutingError (No route matches [GET] "/x")`}},
		{"memory addresses",
			[]string{"Error: undefined method 'name' for #<User:0x000000012a3b4c5d>"},
			[]string{"Error: undefined method 'name' for #<User:0x00000001ffee0011>"}},
		{"temp paths",
			[]string{"Error: ENOENT: no such file or directory, open '/tmp/tmp.Xy12ab/config.json'"},
			[]string{"Error: ENOENT: no such file or directory, open '/tmp/tmp.Pq98zz/config.json'"}},
		{"macOS temp paths",
			[]string{"Error: cannot read /var/folders/7x/abc123/T/go-build4411/b001/exe/main"},
			[]string{"Error: cannot read /var/folders/7x/abc123/T/go-build9087/b001/exe/main"}},
		{"retry counts",
			[]string{"Error: connection failed after 3 retries"},
			[]string{"Error: connection failed after 5 retries"}},
		{"attempt numbers",
			[]string{"Error: connection failed (attempt 2 of 5)"},
			[]string{"Error: connection failed (attempt 4 of 5)"}},
		{"spacing",
			[]string{"ERROR    Exception in ASGI application"},
			[]string{"ERROR  Exception in ASGI application  "}},
		{"the context around it changes",
			[]string{"TypeError: x is undefined", "GET /a 200", "GET /b 200"},
			[]string{"TypeError: x is undefined", "POST /c 500"}},
		{"a named JavaScript frame moved",
			[]string{"TypeError: x is undefined", "    at NoticeList (app/page.tsx:14:22)"},
			[]string{"TypeError: x is undefined", "    at NoticeList (app/page.tsx:15:3)"}},
		{"a named Python frame moved",
			[]string{"Traceback (most recent call last):", `  File "/app/main.py", line 23, in read_notices`, "    return [n for n in xs]", "TypeError: 'NoneType' object is not iterable"},
			[]string{"Traceback (most recent call last):", `  File "/app/main.py", line 25, in read_notices`, "    return [n for n in xs]", "TypeError: 'NoneType' object is not iterable"}},
		{"a named Ruby frame moved",
			[]string{"NoMethodError: undefined method 'name' for nil", "\tfrom app/models/user.rb:12:in 'User#display'"},
			[]string{"NoMethodError: undefined method 'name' for nil", "\tfrom app/models/user.rb:19:in 'User#display'"}},
		{"a Go panic with new goroutine, addresses and lines",
			[]string{"panic: runtime error: index out of range", "", "goroutine 34 [running]:", "main.handle(0xc0001a2000)", "\t/app/main.go:42 +0x2e"},
			[]string{"panic: runtime error: index out of range", "", "goroutine 7 [running]:", "main.handle(0xc000182000)", "\t/app/main.go:44 +0x31"}},
		{"Elixir frames moved",
			[]string{"** (RuntimeError) boom", "    (my_app 0.1.0) lib/my_app/worker.ex:12: MyApp.Worker.run/1"},
			[]string{"** (RuntimeError) boom", "    (my_app 0.1.0) lib/my_app/worker.ex:15: MyApp.Worker.run/1"}},
		{"the same long line, different past the limit",
			[]string{"Error: " + strings.Repeat("x", 2000) + " one"},
			[]string{"Error: " + strings.Repeat("x", 2000) + " two"}},
		{"a long line whose volatile part changes length",
			[]string{"Error: took 5ms " + strings.Repeat("x", 1100)},
			[]string{"Error: took 1503ms " + strings.Repeat("x", 1100)}},
		{"mktemp paths",
			[]string{"Error: cannot open /tmp/tmp.Xy12ab"},
			[]string{"Error: cannot open /tmp/tmp.Pqrzzw"}},
		{"Python temp files",
			[]string{"Error: bad JSON in /tmp/tmpkxjqwe.json"},
			[]string{"Error: bad JSON in /tmp/tmpabcdef.json"}},
		{"an error with no stack, then unrelated output with one",
			[]string{"Error: EADDRINUSE"},
			[]string{"Error: EADDRINUSE", "GET / 200", "POST /x 200", "GET /y 200", "TypeError: nope", "    at foo (a.js:1:2)"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if n, prints := sameFailure(t, tt.a, tt.b); n != 1 {
				t.Fatalf("got %d entries, want 1. Fingerprints:\n%s", n, strings.Join(prints, "\n---\n"))
			}
		})
	}
}

func TestDifferentFailuresStaySeparate(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
	}{
		{"different messages",
			[]string{"Error: connection refused"},
			[]string{"Error: permission denied"}},
		{"different error types",
			[]string{"TypeError: x is not a function"},
			[]string{"RangeError: x is not a function"}},
		{"small numbers in the message",
			[]string{"Error: expected 2 arguments, got 3"},
			[]string{"Error: expected 3 arguments, got 1"}},
		{"status codes",
			[]string{"Error: request failed with status 500"},
			[]string{"Error: request failed with status 404"}},
		{"array indexes in a message",
			[]string{"TypeError: rows[5] is undefined"},
			[]string{"TypeError: rows[6] is undefined"}},
		{"compile errors on different lines",
			[]string{"src/app.ts:14:22 - error TS2339: Property 'name' does not exist on type 'User'."},
			[]string{"src/app.ts:40:7 - error TS2339: Property 'name' does not exist on type 'User'."}},
		{"Go compile errors on different lines",
			[]string{"./main.go:12:5: undefined: handler"},
			[]string{"./main.go:31:9: undefined: handler"}},
		{"same message thrown from a different function",
			[]string{"TypeError: x is undefined", "    at NoticeList (app/page.tsx:14:22)"},
			[]string{"TypeError: x is undefined", "    at UserList (app/page.tsx:14:22)"}},
		{"same message thrown from a different file",
			[]string{"TypeError: x is undefined", "    at render (app/notices.tsx:14:22)"},
			[]string{"TypeError: x is undefined", "    at render (app/users.tsx:14:22)"}},
		{"an anonymous frame on a different line",
			[]string{"TypeError: x is undefined", "    at /app/server.js:14:22"},
			[]string{"TypeError: x is undefined", "    at /app/server.js:40:1"}},
		{"Python module level code on a different line",
			[]string{"Traceback (most recent call last):", `  File "/app/main.py", line 3, in <module>`, "KeyError: 'DATABASE_URL'"},
			[]string{"Traceback (most recent call last):", `  File "/app/main.py", line 9, in <module>`, "KeyError: 'DATABASE_URL'"}},
		{"Python tracebacks ending in different exceptions",
			[]string{"Traceback (most recent call last):", `  File "/app/main.py", line 23, in read_notices`, "TypeError: 'NoneType' object is not iterable"},
			[]string{"Traceback (most recent call last):", `  File "/app/main.py", line 23, in read_notices`, "KeyError: 'title'"}},
		{"Ruby top level code on a different line",
			[]string{"NameError: uninitialized constant Foo", "\tfrom config/application.rb:3:in '<main>'"},
			[]string{"NameError: uninitialized constant Foo", "\tfrom config/application.rb:8:in '<main>'"}},
		{"Go panics in different functions",
			[]string{"panic: runtime error: index out of range", "", "goroutine 1 [running]:", "main.handle(0xc0001a2000)", "\t/app/main.go:42 +0x2e"},
			[]string{"panic: runtime error: index out of range", "", "goroutine 1 [running]:", "main.serve(0xc0001a2000)", "\t/app/main.go:42 +0x2e"}},
		{"temp files with different names",
			[]string{"Error: cannot parse /tmp/build/config.json"},
			[]string{"Error: cannot parse /tmp/build/routes.json"}},
		{"a project's own tmp directory",
			[]string{"Error: cannot load /app/tmp/cache/a.rb"},
			[]string{"Error: cannot load /app/tmp/cache/b.rb"}},
		{"array indexes before a colon",
			[]string{"TypeError: items[3]: expected string"},
			[]string{"TypeError: items[7]: expected string"}},
		{"test assertions with counts",
			[]string{"Error: expected mock to be called 2 times, but was called 3 times"},
			[]string{"Error: expected mock to be called 1 times, but was called 0 times"}},
		{"issue numbers in brackets",
			[]string{"Error: known bug [#123]"},
			[]string{"Error: known bug [#456]"}},
		{"Python exceptions under docker compose",
			[]string{"web-1  | Traceback (most recent call last):", `web-1  |   File "/app/m.py", line 3, in a`, "web-1  | KeyError: 'x'"},
			[]string{"web-1  | Traceback (most recent call last):", `web-1  |   File "/app/m.py", line 3, in a`, "web-1  | ValueError: 'x'"}},
		{"Python exceptions not named Error",
			[]string{"Traceback (most recent call last):", `  File "/app/m.py", line 3, in a`, "django.core.exceptions.ImproperlyConfigured: no DB"},
			[]string{"Traceback (most recent call last):", `  File "/app/m.py", line 3, in a`, "StopIteration"}},
		{"Go panics in different functions under docker compose",
			[]string{"api-1  | panic: boom", "api-1  | ", "api-1  | goroutine 1 [running]:", "api-1  | main.handle(0xc0001a2000)", "api-1  | \t/app/main.go:42 +0x2e"},
			[]string{"api-1  | panic: boom", "api-1  | ", "api-1  | goroutine 1 [running]:", "api-1  | main.serve(0xc0001a2000)", "api-1  | \t/app/main.go:42 +0x2e"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if n, prints := sameFailure(t, tt.a, tt.b); n != 2 {
				t.Fatalf("got %d entries, want 2. Fingerprints:\n%s", n, strings.Join(prints, "\n---\n"))
			}
		})
	}
}

// Real crash loops, each fed through the engine a line at a time. Every one must
// come out as a single entry counting every crash, and nothing else.
func TestCrashLoopsAreOneEntry(t *testing.T) {
	tests := []struct {
		file      string
		errorLine string // what starts an error in this output
		wantCount int
	}{
		{"node_restart_loop.txt", "Error:", 4},
		{"puma_boot_loop.txt", "! Unable to load application", 3},
		{"go_panic_loop.txt", "panic:", 3},
		{"python_traceback_loop.txt", "Exception in ASGI application", 3},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "group", tt.file))
			if err != nil {
				t.Fatal(err)
			}
			opts := options()
			opts.Matchers = []engine.Matcher{engine.Substring{Label: "crash", Text: tt.errorLine}}
			opts.ContextLines = 20
			a := enginetest.New(opts)
			a.Watch(web, "web", "dev")
			for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
				a.Print(web, line)
				a.Advance(20 * time.Millisecond)
			}
			a.Advance(time.Minute)

			entries := a.Engine.Entries()
			if len(entries) != 1 {
				var prints []string
				for _, en := range entries {
					prints = append(prints, en.Fingerprint)
				}
				t.Fatalf("got %d entries, want 1. Fingerprints:\n%s", len(entries), strings.Join(prints, "\n---\n"))
			}
			if en := entries[0]; en.Count != tt.wantCount || en.State != engine.StateNew {
				t.Fatalf("count %d state %v, want %d and new", en.Count, en.State, tt.wantCount)
			}
		})
	}
}

// A dismissed crash loop that keeps looping stays dismissed, however long it
// runs, because each repeat starts the cooldown again. Once it has been quiet for
// longer than the cooldown, it's a fresh entry.
func TestDismissedCrashLoopStaysQuiet(t *testing.T) {
	a := newAdapter(t)
	a.Print(web, "Error: EADDRINUSE")
	a.Advance(time.Second)
	a.Engine.Dismiss(only(t, a).ID)

	for range 10 {
		a.Advance(30 * time.Second) // every repeat inside the one minute cooldown
		a.Print(web, "Error: EADDRINUSE")
	}
	a.Advance(time.Second)
	en := only(t, a)
	if en.Count != 11 || en.State != engine.StateDismissed {
		t.Fatalf("count %d state %v, want 11 and dismissed", en.Count, en.State)
	}

	a.Advance(2 * time.Minute)
	a.Print(web, "Error: EADDRINUSE")
	a.Advance(time.Second)
	entries := a.Engine.Entries()
	if len(entries) != 2 || entries[0].State != engine.StateNew || entries[0].Count != 1 {
		t.Fatalf("after a quiet spell got %+v, want a fresh entry on top", entries)
	}
}
