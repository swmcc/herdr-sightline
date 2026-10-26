package engine

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The fix check (#12). After an entry is sent, the settle period starts when its
// agent finishes. Then:
//
//   - The same error matched during the settle period: still failing. Sending it
//     again (MarkSent) starts the whole check afresh.
//   - A different error: a new entry, which grouping does by itself.
//   - Neither, with positive evidence that the pane ran the changed code and no
//     uncertain reads: fixed.
//   - Anything else: unverified.
//
// Fixed only ever means the error stopped appearing. It never means the code is
// right, and the rules below lean towards unverified whenever they're unsure.
//
// # What counts as evidence
//
// No new lines never proves no output, and most lines prove nothing: a request
// log says the server is up, not that the new code ran. So evidence is narrow.
// It has to come from the entry's own source, after the agent finished (a line
// at the very moment it finished could be from before, so it doesn't count) and
// within the first half of the settle period, so the error has had at least the
// other half to come back. What it has to be depends on the error:
//
//   - If the error's context shows the request it came from (a request that
//     failed, or one that had just started, see triggerOf), the error probably
//     needed that request to happen. Recompiling proves nothing then, so the
//     only evidence is the same method, path and query being requested again
//     with a 2xx (see sawLine). This is the README's "the bug needs a request
//     to trigger" case, and without the request it stays unverified. A bug
//     that depends on who sends the request can still look fixed when someone
//     else sends it.
//   - Otherwise it's a line saying a compile, build, restart or test run
//     finished or started over. See ranSignals. A compile that started but
//     hasn't finished ("Compiling...") isn't enough.
//
// # Uncertain reads
//
// A read that may have missed output (Gap) or can't say what's new (Unsure)
// while an entry is settling makes that entry unverified, never fixed, because
// the error may have come back unseen. So does output the scrubber swallowed
// (a control string hiding lines) and the source being unwatched. The same goes
// for the error coming back after the settle period but before the verdict. A Gap read's lines may repeat lines from
// before, so a repeat found in them doesn't make an entry still failing either.
// A repeat whose capture was only flagged MaybeGap still counts: its error line
// was read fresh, it's just the context that may have a hole.
//
// # The agent going away
//
// If the agent an entry was sent to goes away before it finishes (its pane
// closed or exited, the agent left the pane or another kind of agent took it
// over), nothing will ever finish it, so AgentGone ends the wait: the entry is
// unverified with a Note saying why, and can be sent again. If the agent had
// already finished and the settle period is running, AgentGone changes nothing.
// The verdict then depends on the pane's output, not on the agent.
//
// Every change to an entry's state after it's caught goes through this file.

// fixState tracks the settle period for one sent entry.
type fixState struct {
	settling bool
	from     time.Time // when the agent finished
	until    time.Time // end of the settle period

	// trigger is the request ("GET /notices") the error seems to have needed,
	// or empty if it doesn't look like it came from a request.
	trigger string
	// started means the trigger has just started again in a form with no
	// status (Rails' Started, Phoenix's "GET /notices"), so the next line
	// saying how a request ended is its outcome.
	started bool
	ran     bool // positive evidence that the changed code ran
	unsure  bool // output may have gone unseen since the settle period started

	// lateRepeat means the error came back after the settle period but before
	// the verdict. It's a new entry, but it still rules out fixed.
	lateRepeat bool

	// gaps are the times of recent Gap reads during the settle period. A repeat
	// matched at one of them came from lines that may be a replay. Only times
	// within longestWait are kept, as captures from older reads have been filed.
	gaps []time.Time
}

// sent records that r went to agent at now. It also restarts a check that's
// under way or finished, which is how a resend after still failing works.
func (e *Engine) sent(r *record, agent string, _ time.Time) {
	r.entry.State = StateSent
	r.entry.Agent = agent
	r.entry.Note = ""
	r.fix = fixState{}
}

// agentFinished starts the settle period for every entry sent to agent and still
// waiting. If one has already started, it keeps its first start, so an agent that
// keeps finishing can't push the verdict out for ever.
func (e *Engine) agentFinished(agent string, now time.Time) {
	for _, r := range e.records {
		if r.entry.State != StateSent || r.entry.Agent != agent || r.fix.settling {
			continue
		}
		r.fix = fixState{
			settling: true,
			from:     now,
			until:    now.Add(e.opts.Settle),
			trigger:  triggerOf(r.entry),
		}
	}
}

// NoteAgentGone is the Note on an entry whose agent went away before it
// finished.
const NoteAgentGone = "the agent went away before finishing"

// agentGone ends the wait for every entry sent to agent that it hasn't
// finished. An entry whose settle period has started carries on, as its
// verdict comes from the pane's output.
func (e *Engine) agentGone(agent string, _ time.Time) {
	for _, r := range e.records {
		if r.entry.State != StateSent || r.entry.Agent != agent || r.fix.settling {
			continue
		}
		r.entry.State = StateUnverified
		r.entry.Note = NoteAgentGone
		r.fix = fixState{}
	}
}

// dismissed records that the person dismissed r.
func (e *Engine) dismissed(r *record) {
	r.entry.State = StateDismissed
	r.entry.Note = ""
	r.fix = fixState{}
}

// sawLine is called for every clean line from a watched source, after detection
// has seen it. It looks for evidence that the changed code ran.
func (e *Engine) sawLine(src SourceID, line string, now time.Time) {
	// The line is only looked at once an entry could use it, so most lines
	// cost a walk over the entries and no matching.
	read := false
	var signal bool
	var request string
	var status, ended int
	for _, r := range e.records {
		f := &r.fix
		if !f.settling || f.ran || r.entry.Source.ID != src {
			continue
		}
		// Half of this settle period's own length, as Reconfigure may have
		// changed Settle since it started.
		if !now.After(f.from) || now.After(f.from.Add(f.until.Sub(f.from)/2)) {
			continue
		}
		if !read {
			l := unprefix(cut(line, maxScannedLine))
			request, status = requestOf(l)
			ended = endedWith(l)
			// Anyone who can reach the server can put words in its request
			// log ("GET /compiled/successfully 404"), so a line that mentions
			// a request anywhere, whatever its method, is never a sign that
			// something ran.
			signal = !mentionsRequest.MatchString(l) && ranSignal(l)
			read = true
		}
		if f.trigger == "" {
			f.ran = signal
			continue
		}
		// Only a 2xx counts. A 5xx is the request failing again, and anyone
		// can send the same request in a way that never reaches the bug: one
		// that's redirected to a login page, rejected as bad or not found.
		switch {
		case request == f.trigger && status == 0:
			f.started = true
		case request == f.trigger:
			f.ran = succeeded(status)
		case request != "":
			// Another request started, so the next ending may be its.
			f.started = false
		case ended != 0 && f.started:
			f.ran = succeeded(ended)
			f.started = false
		}
	}
}

// uncertainRead records that a read from src at now may have missed output
// (gap) or couldn't tell what was new. It's called after any capture due by now
// has been filed.
func (e *Engine) uncertainRead(src SourceID, now time.Time, gap bool) {
	for _, r := range e.records {
		f := &r.fix
		if !f.settling || r.entry.Source.ID != src {
			continue
		}
		f.unsure = true
		if gap {
			oldest := now.Add(-e.longestWait())
			f.gaps = slices.DeleteFunc(f.gaps, func(t time.Time) bool { return t.Before(oldest) })
			f.gaps = append(f.gaps, now)
		}
	}
}

// pastSettle reports whether an error matched at matchedAt came too late to
// count toward r's settle period. Grouping makes such a repeat a new entry. It's
// decided by match time, so the outcome never depends on when Tick runs.
//
// It also notes the late repeat, as grouping only asks when the same error has
// come back. An entry whose error came back before its verdict, even too late to
// make it still failing, mustn't be called fixed.
func (e *Engine) pastSettle(r *record, matchedAt time.Time) bool {
	past := r.entry.State == StateSent && r.fix.settling && !matchedAt.Before(r.fix.until)
	if past {
		r.fix.lateRepeat = true
	}
	return past
}

// recurred is called when an entry's error is seen again at matchedAt.
func (e *Engine) recurred(r *record, matchedAt time.Time) {
	// Only a repeat inside the settle period counts. One after it ended, but
	// before the verdict was given, doesn't.
	f := r.fix
	if r.entry.State != StateSent || !f.settling || matchedAt.Before(f.from) || !matchedAt.Before(f.until) {
		return
	}
	// A repeat from a Gap read may be the old error printed again, so it can't
	// say still failing. The gap has already made the entry unsure.
	if slices.ContainsFunc(f.gaps, matchedAt.Equal) {
		return
	}
	r.entry.State = StateStillFailing
	r.fix = fixState{}
}

// checkFixes gives a verdict on every entry whose settle period is over. It
// waits a further CaptureWait (or longer, see longestWait), so a repeat that
// matched just before the end has been filed (and has made the entry still
// failing) before the verdict.
func (e *Engine) checkFixes(now time.Time) {
	wait := time.Duration(-1) // only worked out once an entry is settling
	for _, r := range e.records {
		f := r.fix
		if !f.settling {
			continue
		}
		if wait < 0 {
			wait = e.longestWait()
		}
		if now.Before(f.until.Add(wait)) {
			continue
		}
		if f.ran && !f.unsure && !f.lateRepeat {
			r.entry.State = StateFixed
		} else {
			r.entry.State = StateUnverified
		}
		r.fix = fixState{}
	}
}

// ranSignals matches a line saying a compile, build, restart or test run has
// finished or started over: proof that something ran the code since the agent
// finished. It's deliberately short. A line that only shows the process is
// alive (a request log, "watching for changes") or that work has merely begun
// ("Compiling...") isn't here. A signal that's missing only means unverified
// where fixed would have been fair, which is the right way to be wrong.
var ranSignals = regexp.MustCompile(`(?i)` + strings.Join([]string{
	// webpack, Next.js, Create React App: "compiled successfully in 1.2s",
	// "✓ Compiled /notices in 312ms", "compiled with 1 warning".
	`\bcompiled\b.*\bsuccessfully\b`,
	`^\W*compiled \S+ in \d[\d.]*\s?m?s\b`,
	`\bcompiled with \d+ warnings?\b`,
	// Vite and Next.js starting up: "ready in 312 ms", "✓ Ready in 1.2s".
	`\bready in \d[\d.]*\s?m?s\b`,
	// Vite reloading changed code: "[vite] hmr update /src/App.tsx".
	`\[vite\] (?:hmr update|page reload)\b`,
	// Builds: "built in 1.23s", "[watch] build finished", "rebuilt in 80ms".
	`\b(?:re)?built in \d[\d.]*\s?m?s\b`,
	`\bbuild finished(?:,| in |\s*$)`, // not "build finished with 2 errors"
	// tsc --watch: "Found 0 errors. Watching for file changes."
	`\bfound \d+ errors?\b.*\bwatching for file changes\b`,
	// cargo: "Finished `dev` profile [unoptimized] target(s) in 0.52s".
	"^\\s*finished\\s+\\S.*\\btarget\\(s\\) in \\d[\\d.]*s\\s*$",
	// Phoenix: "Generated my_app app".
	`^\s*generated \S+ app\s*$`,
	// Servers restarting or back up.
	`^\s*\[nodemon\] (?:restarting|starting)\b`,
	// Django's autoreloader: "/app/notices/views.py changed, reloading." The
	// path has no colons or spaces, so "INFO:root:evil.py" can't pass for one.
	`^\s*/?(?:[\w.-]+/)*[\w.-]+\.py changed, reloading\.?\s*$`,
	`^\W*listening on\b`, // puma: "* Listening on http://127.0.0.1:3000"
	`\bstarting development server at\b`,
	`\bapplication startup complete\b`, // uvicorn
	`\brunning \S+\.endpoint with\b`,   // Phoenix
	// Test runs finishing, passed or failed.
	`^\s*tests:\s+\d+ `,                                                     // Jest
	`^\s*tests\s+\d+ (?:passed|failed)\b`,                                   // Vitest
	`(?-i:^(?:PASS|FAIL)\s+\S)`,                                             // Jest, per file
	`(?-i:^(?:ok|FAIL)\s+\S+\s+\d[\d.]*s\s*$)`,                              // go test, not "(cached)"
	`^=+ .*\b\d+ (?:passed|failed)\b.* in \d[\d.]*s\b`,                      // pytest
	`^\d+ examples?, \d+ failures?\b`,                                       // RSpec
	`^\d+ runs?, \d+ assertions?, \d+ failures?\b`,                          // Minitest
	`^(?:\d+ (?:doctests?|propert(?:y|ies)), )*\d+ tests?, \d+ failures?\b`, // ExUnit
	`^test result: (?:ok|failed)\. \d+ passed\b`,                            // cargo test
}, "|"))

// ranSignal reports whether l says something ran. The signals are tools' own
// words, and tools print them at the start of a line, after at most a symbol,
// a time, tags in brackets, a log level like uvicorn's "INFO:" and the tool's
// name and version (see leadIn). A match after anything else doesn't count,
// because it could be text someone else chose that the server logged: a path
// ("Not Found: /compiled/successfully"), a query
// (`Parameters: {"q"=>"compiled successfully"}`), a record id ("Couldn't find
// User with 'id'=x compiled successfully"), a search term or a request id
// inside a tag ("[compiled-successfully] Processing by").
//
// It can't stop every forgery. A server that logs someone else's text at the
// very start of a line can still print one that looks like a recompile, and so
// can one that logs it as a whole tag of its own, or logs a newline someone
// sent as it is so their text starts a line. The same goes for requests (see
// requestForms). That's why fixed is only ever a hint.
func ranSignal(l string) bool {
	for _, m := range ranSignals.FindAllStringIndex(l, -1) {
		if leadIn.MatchString(l[:m[0]]) {
			return true
		}
	}
	return false
}

// The parts of a log line that may come before its own words, shared by the
// signals and the request forms.
const (
	// 10:41:03 AM - or 10:41:03.120
	logTime = `(?:\d{1,2}:\d{2}:\d{2}(?:\.\d+)?(?: ?[AaPp][Mm])?\s*(?:-\s*)?)?`
	// [vite] [watch] [info] [abc-123]. Closed, with no spaces, so a signal
	// can't start inside one.
	logTags = `(?:\[[\w.:@-]*\]\s*)*`
	// uvicorn's INFO:
	logLevel = `(?:(?:INFO|DEBUG|NOTICE):\s*)?`
)

// leadIn is what may come before a signal on its line: spaces and at most one
// symbol ("✓", "*", "▲", "√", ">"), then a time, tags, a log level and the
// tool's name. The symbol is a symbol and not just anything that isn't a word,
// so it can't be a bracket, a quote or a word in another script
// ("Пользователь").
var leadIn = regexp.MustCompile(`^\s*(?:[\p{So}*•>√]\s*)?` + logTime + logTags + logLevel +
	`(?:(?:webpack|VITE)(?: v?\d\S*)?\s+)?$`) // webpack 5.88.2, VITE v5.4.2

// mentionsRequest finds a request anywhere in a line, whatever its method:
// "PURGE /x", `Started GET "/x"` or a quoted request line like
// `"PROPFIND / HTTP/1.1"`, whose method can be any token. A line with one is
// never a signal, because a server may log text the client chose next to it
// (a forwarded address, a host, a user).
var mentionsRequest = regexp.MustCompile(`\b[A-Z][A-Z0-9_-]+ +"?/|"[^\s"]+ \S+ HTTP/\d`)

// requestForms are the request lines dev servers print. Each is anchored at
// the start of the line, after a time, tags or a log level, so a request
// mentioned anywhere else on a line (a logged parameter like `Parameters:
// {"q"=>"GET /notices 200"}` or Django's "Not Found: /x GET /notices 200")
// never reads as one. Each has the method, the path and maybe the status as
// its groups. The query string is part of the path, as a bug may need it.
var requestForms = func() []*regexp.Regexp {
	const (
		start  = `^\s*` + logTime + logTags + logLevel
		method = `(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS) `
		path   = `(/[^\s"#]*)`
		status = `([1-5]\d\d)`
		// The quoted part of a request line, with no quote or space inside.
		request = method + path + `[^\s"]* HTTP/(?:1\.[01]|0\.9)`
		// An IPv4 or IPv6 address, with or without a port.
		address = `[\w.:\[\]-]+`
	)
	return []*regexp.Regexp{
		// Next.js, Phoenix, morgan's dev and tiny formats: "GET /notices 500 in
		// 30ms", "[info] GET /notices". The path is the only part a client
		// chooses, and it has no spaces.
		regexp.MustCompile(start + method + path + `\S*(?:\s+` + status + `\b)?`),
		// Rails: `Started GET "/notices" for 127.0.0.1`, with no status
		regexp.MustCompile(start + `Started ` + method + `"` + path + `[^\s"]*"()`),
		// A quoted request line. Servers log fields a client chooses before it
		// (a user, a forwarded address), and some log a bad request line raw,
		// quotes and all. So these forms are anchored at both ends, with the
		// server's own status and size or reason last. The fields before the
		// quoted request are fixed in place and can't hold a space, and the
		// request itself can't hold a quote or a space.
		//
		// The common log format, Werkzeug's included:
		// `127.0.0.1 - - [01/Jan/2026:09:00:00 +0000] "GET /notices HTTP/1.1" 500 512`
		regexp.MustCompile(start + address + ` \S+ \S+ \[[^\]]*\] "` + request + `" ` + status + ` (?:\d+|-)\s*$`),
		// Django: `[01/Jan/2026 09:00:00] "GET /notices HTTP/1.1" 500 145`
		regexp.MustCompile(start + `\[[^\]]*\] "` + request + `" ` + status + ` (?:\d+|-)\s*$`),
		// uvicorn: `INFO:     127.0.0.1:52342 - "GET /notices HTTP/1.1" 500 Internal Server Error`
		regexp.MustCompile(start + address + ` - "` + request + `" ` + status + `(?: [A-Za-z' -]*)?\s*$`),
	}
}()

// requestOf gives "GET /notices?page=2" and its status for a request line, with 0 when
// the line shows no status. The key is empty if it isn't a request line.
func requestOf(l string) (key string, status int) {
	// A server logs one request line per request. A second means someone put
	// one in a field of their own.
	if strings.Count(l, "HTTP/") > 1 {
		return "", 0
	}
	for _, re := range requestForms {
		m := re.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if m[3] != "" {
			status, _ = strconv.Atoi(m[3])
		}
		return m[1] + " " + m[2], status
	}
	return "", 0
}

// requestEnded is the line Rails and Phoenix print when a request they logged
// without a status ends: "Completed 200 OK in 12ms", "[info] Sent 200 in 3ms".
// It doesn't say which request ended, so the fix check takes the next one
// after the trigger starts, and none if another request starts in between. A
// server handling requests side by side can still end someone else's request
// there.
var requestEnded = regexp.MustCompile(`^\s*` + logTime + logTags + logLevel + `(?:Completed|Sent) ([1-5]\d\d)\b`)

// endedWith gives the status on a line saying a request ended, or 0.
func endedWith(l string) int {
	m := requestEnded.FindStringSubmatch(l)
	if m == nil {
		return 0
	}
	status, _ := strconv.Atoi(m[1])
	return status
}

// succeeded reports whether status is a 2xx.
func succeeded(status int) bool {
	return status >= 200 && status < 300
}

// unclearTrigger is the trigger of an error that seems to have needed a
// request, when the context can't say which. No request line has it as its
// key, so such an entry is never fixed.
const unclearTrigger = "?"

// triggerOf is the request en's error seems to have needed, or empty if it
// doesn't look like it came from one. Dev servers log plenty of requests that
// have nothing to do with an error (health checks, polling, favicons), and
// anyone who can reach the server can put their own requests next to an error
// and then send them again to pass. So a request only counts if it's the one
// that failed:
//
//  1. the nearest request near the error with a 5xx status, looking back
//     through the lines before it and then on through the lines after it
//  2. otherwise the request before it that shows no status, which is a
//     request starting (Rails' `Started GET "/notices"`, Phoenix's "GET
//     /notices"), when it ended with a 5xx or its end isn't shown. One that
//     ended any other way had nothing to do with the error.
//
// If the context holds any two requests with no status, before or after the
// error and ended or not, or an ending that can't be tied to a request, the
// endings may be on the wrong requests. A request's start can scroll out of
// the context, so two that look one after the other may still have run side
// by side. There's no telling which request failed then, and the trigger is
// unclearTrigger, even when no request seems to have failed. So a Rails or
// Phoenix error with two requests in its context is only ever unverified,
// even after a recompile. A 4xx is never the trigger, as anyone can make one,
// and nor is the server's own 501 or 505.
//
// One request someone else sent just before an error that came from no request
// at all, and that hasn't ended by the time the context is captured, still
// becomes the trigger. So does one they started after the failing request's
// own start scrolled out of the context, which a request logging more than
// ContextLines lines before it fails can do. Those are among the reasons fixed
// is only a hint.
func triggerOf(en Entry) string {
	type seen struct {
		key    string
		status int
		ended  int // for a request with no status, how it ended, if shown
		at     int // where it is in the lines
	}
	lines := append(append(slices.Clip(en.Before), en.Lines...), en.After...)
	var reqs []seen // in the order printed
	// stray means an ending couldn't be tied to a request, so requests were
	// handled side by side and endings may be on the wrong ones.
	stray := false
	for i, l := range lines {
		l = unprefix(cut(l, maxScannedLine))
		if k, st := requestOf(l); k != "" {
			reqs = append(reqs, seen{key: k, status: st, at: i})
			continue
		}
		// An ending belongs to the last request, if that showed no status:
		// one that started since would be last instead.
		if end := endedWith(l); end != 0 && len(reqs) > 0 {
			if last := &reqs[len(reqs)-1]; last.status == 0 && last.ended == 0 {
				last.ended = end
			} else {
				stray = true
			}
		}
	}
	// The server's own refusals (501 for a method it doesn't know, 505 for an
	// HTTP version it doesn't speak) never reach the app, and anyone can get
	// one, so they're not a request failing.
	failed := func(status int) bool {
		return status >= 500 && status != 501 && status != 505
	}
	errorAt := len(en.Before)
	for _, r := range slices.Backward(reqs) {
		if r.at < errorAt && failed(r.status) {
			return r.key
		}
	}
	for _, r := range reqs {
		if r.at >= errorAt && failed(r.status) {
			return r.key
		}
	}
	var candidates []string
	for _, r := range reqs {
		if r.at < errorAt && r.status == 0 && (r.ended == 0 || r.ended >= 500) {
			candidates = append(candidates, r.key)
		}
	}
	// An ending doesn't say which request it ends, so with any two requests
	// with no status in the context, an ending may be on the wrong one, and
	// someone sending requests of their own can arrange that it is.
	statusless := 0
	for _, r := range reqs {
		if r.status == 0 {
			statusless++
		}
	}
	switch {
	case statusless > 1 || stray:
		return unclearTrigger
	case len(candidates) == 0:
		return ""
	}
	return candidates[0]
}
