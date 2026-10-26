package engine

import (
	"regexp"
	"slices"
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
//   - If a request line ("GET /notices", `Started GET "/notices"`) sits near the
//     error in its context, the error probably needed that request to happen.
//     Recompiling proves nothing then, so the only evidence is the same method
//     and path being requested again. This is the README's "the bug needs a
//     request to trigger" case, and without the request it stays unverified.
//   - Otherwise it's a line saying a compile, build, restart or test run
//     finished or started over. See ranSignals. A compile that started but
//     hasn't finished ("Compiling...") isn't enough.
//
// # Uncertain reads
//
// A read that may have missed output (Gap) or can't say what's new (Unsure)
// while an entry is settling makes that entry unverified, never fixed, because
// the error may have come back unseen. A Gap read's lines may repeat lines from
// before, so a repeat found in them doesn't make an entry still failing either.
// A repeat whose capture was only flagged MaybeGap still counts: its error line
// was read fresh, it's just the context that may have a hole.
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
	ran     bool // positive evidence that the changed code ran
	unsure  bool // a Gap or Unsure read since the settle period started

	// gaps are the times of recent Gap reads during the settle period. A repeat
	// matched at one of them came from lines that may be a replay. Only times
	// within CaptureWait are kept, as captures from older reads have been filed.
	gaps []time.Time
}

// sent records that r went to agent at now. It also restarts a check that's
// under way or finished, which is how a resend after still failing works.
func (e *Engine) sent(r *record, agent string, _ time.Time) {
	r.entry.State = StateSent
	r.entry.Agent = agent
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

// dismissed records that the person dismissed r.
func (e *Engine) dismissed(r *record) {
	r.entry.State = StateDismissed
	r.fix = fixState{}
}

// sawLine is called for every clean line from a watched source, after detection
// has seen it. It looks for evidence that the changed code ran.
func (e *Engine) sawLine(src SourceID, line string, now time.Time) {
	l := unprefix(cut(line, maxScannedLine))
	signal := ranSignals.MatchString(l)
	request := requestKey(l)
	if !signal && request == "" {
		return
	}
	for _, r := range e.records {
		f := &r.fix
		if !f.settling || f.ran || r.entry.Source.ID != src {
			continue
		}
		if !now.After(f.from) || now.After(f.from.Add(e.opts.Settle/2)) {
			continue
		}
		if f.trigger != "" {
			f.ran = request == f.trigger
		} else {
			f.ran = signal
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
			oldest := now.Add(-e.opts.CaptureWait)
			f.gaps = slices.DeleteFunc(f.gaps, func(t time.Time) bool { return t.Before(oldest) })
			f.gaps = append(f.gaps, now)
		}
	}
}

// pastSettle reports whether an error matched at matchedAt came too late to
// count toward r's settle period. Grouping makes such a repeat a new entry. It's
// decided by match time, so the outcome never depends on when Tick runs.
func (e *Engine) pastSettle(r *record, matchedAt time.Time) bool {
	return r.entry.State == StateSent && r.fix.settling && !matchedAt.Before(r.fix.until)
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
// waits a further CaptureWait, so a repeat that matched just before the end has
// been filed (and has made the entry still failing) before the verdict.
func (e *Engine) checkFixes(now time.Time) {
	for _, r := range e.records {
		f := r.fix
		if !f.settling || now.Before(f.until.Add(e.opts.CaptureWait)) {
			continue
		}
		if f.ran && !f.unsure {
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
	`\bcompiled with \d+ (?:warnings?|errors?)\b`,
	// Vite and Next.js starting up: "ready in 312 ms", "✓ Ready in 1.2s".
	`\bready in \d[\d.]*\s?m?s\b`,
	// Vite reloading changed code: "[vite] hmr update /src/App.tsx".
	`\[vite\] (?:hmr update|page reload)\b`,
	// Builds: "built in 1.23s", "[watch] build finished", "rebuilt in 80ms".
	`\b(?:re)?built in \d[\d.]*\s?m?s\b`,
	`\bbuild finished\b`,
	// tsc --watch: "Found 0 errors. Watching for file changes."
	`\bfound \d+ errors?\b.*\bwatching for file changes\b`,
	// cargo: "Finished `dev` profile [unoptimized] target(s) in 0.52s".
	"^\\s*finished\\b.*\\bin \\d[\\d.]*s\\s*$",
	// Phoenix: "Generated my_app app".
	`^\s*generated \S+ app\s*$`,
	// Servers restarting or back up.
	`^\s*\[nodemon\] (?:restarting|starting)\b`,
	`\bchanged, reloading\b`, // Django's autoreloader
	`^\W*listening on\b`,     // puma: "* Listening on http://127.0.0.1:3000"
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

// requestLine finds the method and path of an HTTP request in a log line, in the
// forms dev servers print: "GET /notices 500 in 30ms", `Started GET "/notices"`,
// `"GET /notices HTTP/1.1" 500`. The query string isn't part of it.
var requestLine = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS) "?(/[^\s"?#]*)`)

// requestKey is "GET /notices" for a request line, or empty.
func requestKey(l string) string {
	m := requestLine.FindStringSubmatch(l)
	if m == nil {
		return ""
	}
	return m[1] + " " + m[2]
}

// triggerOf is the request en's error seems to have needed: the nearest request
// line before it, or failing that the first after it. Empty means the error
// doesn't look like it came from a request.
func triggerOf(en Entry) string {
	for _, l := range slices.Backward(en.Before) {
		if k := requestKey(unprefix(cut(l, maxScannedLine))); k != "" {
			return k
		}
	}
	for _, l := range en.After {
		if k := requestKey(unprefix(cut(l, maxScannedLine))); k != "" {
			return k
		}
	}
	return ""
}
