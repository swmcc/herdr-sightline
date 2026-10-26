package engine

import (
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Grouping decides whether a caught error is one the feed already has. The same
// failure printed forty times in a crash loop should be one entry with a count of
// forty, even though every copy has its own timestamp, PID and request id.
//
// The fingerprint is the error line plus the first few stack frames after it,
// with the parts that change from run to run swapped for placeholders. It never
// uses the rest of the context, because the lines around an error (a request log,
// a progress bar) change for reasons that have nothing to do with the error.

// groups is the index behind grouping: the newest entry for each source and
// fingerprint, so a repeat is found without scanning every entry. An entry's key
// can always be rebuilt from the entry itself (see keyOf), so dropping an entry
// (#41) only has to delete its key if the key still points at it.
type groups struct {
	newest map[groupKey]*record
}

type groupKey struct {
	source      SourceID
	fingerprint string
}

// keyOf is the index key for r. The source id never changes on a repeat, and
// neither does the fingerprint, so it's the key r was filed under.
func keyOf(r *record) groupKey {
	return groupKey{r.entry.Source.ID, r.entry.Fingerprint}
}

// maxFrames is how many stack frames go into the fingerprint. A few frames tell
// apart two errors with the same message thrown from different places. More would
// let a deep, slightly different call path split one failure in two.
const maxFrames = 3

// maxLeadIn is how many lines that aren't frames may come between the error line
// and its first frame. Go puts a signal line, a blank line and a goroutine header
// there, and a Rails boot failure puts a bundler line and the raw exception. Any
// further away and the frames are someone else's.
const maxLeadIn = 3

// Output is untrusted and a line can be any length. Each line is cut to
// maxScannedLine before it's looked at, then to maxFingerprintLine once the
// volatile parts are out. Cutting only after normalising means a timestamp or PID
// that changes length can't move where the cut falls.
const (
	maxScannedLine     = 4096
	maxFingerprintLine = 1024
)

// fingerprint says which failure f is. Two errors with the same fingerprint from
// the same source are the same failure. It's also what the fix check (#12) uses to
// tell the same error coming back from a different one.
//
// It only sees the lines the window captured after the error, so a Python
// traceback longer than ContextLines loses its exception line and its innermost
// frames, and different exceptions from deep in one framework can share a
// fingerprint. A look ahead of its own would need the window's help (#8).
func fingerprint(f found) string {
	if len(f.lines) == 0 {
		return ""
	}
	first := unprefix(cut(f.lines[0], maxScannedLine))
	rest := append(append([]string(nil), f.lines[1:]...), f.after...)
	parts := append([]string{normalise(first)}, frames(rest, pythonTraceback.MatchString(first))...)
	return strings.Join(parts, "\n")
}

var (
	// linePrefix is what a multiplexer puts in front of every line: docker
	// compose's "web-1  | " or the "    | " of a Python exception group. It's the
	// same on every line, so it's taken off before anything else is matched.
	linePrefix = regexp.MustCompile(`^\s*(?:[\w.\-]+\s+)?\| ?`)
	// pythonTraceback starts a Python traceback. Python prints the innermost frame
	// last and the exception after it, so a traceback is read differently.
	pythonTraceback = regexp.MustCompile(`Traceback \(most recent call last\):\s*$`)
	// pythonException is the line that ends a traceback: "ValueError: bad input",
	// "django.core.exceptions.ImproperlyConfigured" or a bare "StopIteration".
	pythonException = regexp.MustCompile(`^[A-Za-z_][\w.]*(?::|$)`)
	// goHeader is the line before a Go panic's frames: "goroutine 7 [running]:".
	goHeader = regexp.MustCompile(`^goroutine \d+ \[.*\]:$`)
)

// unprefix takes off a multiplexer's prefix (see linePrefix).
func unprefix(l string) string {
	return strings.TrimPrefix(l, linePrefix.FindString(l))
}

// frames picks out the stack frames from the lines after an error, normalised.
// The frames have to start within maxLeadIn lines, and the first line that can't
// be part of a trace ends them, so frames from unrelated output that happens to
// follow never get in.
//
// Usually that's the first few frames, nearest the error. In a Python traceback
// it's the last few, which are the nearest, plus the exception line that ends it,
// because the line that matched may only have been the traceback's header.
func frames(lines []string, python bool) []string {
	var out []string
	n := 0      // frames started, as a Go frame takes two lines
	leadIn := 0 // lines before the first frame that weren't frames
	afterGoFunc := false
	for _, l := range lines {
		l = unprefix(cut(l, maxScannedLine))
		if afterGoFunc && goFileFrame.MatchString(l) {
			// The second line of a Go frame. The function is on the line above, so
			// it's clearly the same frame and the line number can go.
			out = append(out, normalise(goFileFrame.ReplaceAllString(l, "$1")))
			afterGoFunc = false
			continue
		}
		afterGoFunc = false
		var f string
		var ok bool
		if goFuncFrame.MatchString(l) {
			f, ok, afterGoFunc = l, true, true
		} else {
			f, ok = frame(l)
		}
		if ok {
			if !python && n == maxFrames {
				break
			}
			out = append(out, normalise(f))
			n++
			continue
		}
		if n == 0 {
			// Still looking for the trace. A Traceback header starts one.
			if pythonTraceback.MatchString(l) {
				python = true
				continue
			}
			if leadIn++; leadIn > maxLeadIn {
				break
			}
			continue
		}
		// Inside a trace. Python shows the source line and carets indented under
		// each frame, and Go leaves a blank line between goroutines.
		if python {
			if strings.TrimSpace(l) != "" && strings.TrimLeft(l, " \t") == l {
				if pythonException.MatchString(l) {
					return append(last(out, maxFrames), normalise(l))
				}
				break
			}
			continue
		}
		if strings.TrimSpace(l) == "" || goHeader.MatchString(l) {
			continue
		}
		break
	}
	if python {
		return last(out, maxFrames)
	}
	return out
}

// last returns the last n of s.
func last(s []string, n int) []string {
	return s[max(len(s)-n, 0):]
}

// frame reports whether l is a stack frame, and gives it back with its line and
// column numbers taken out when the frame names its function.
//
// Line numbers in frames are a judgement call. A frame that names its function
// and file is clearly the same frame if only the line moved, which is what
// happens when an agent edits code above the failing line: the error is still the
// same one, and the fix check should say still failing rather than fixed. A frame
// with no function name (anonymous, module level or top level code) has only its
// file and line to say where it is, so the line stays. Line numbers in the error
// line itself always stay, because there they usually tell two different compile
// errors in the same file apart.
func frame(l string) (string, bool) {
	for _, p := range framePatterns {
		m := p.re.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if m[p.fn] == "" || strings.Contains(m[p.fn], "<") {
			// No function, or <anonymous>, <module>, <main>, <top (required)>.
			return l, true
		}
		return p.re.ReplaceAllString(l, p.keep), true
	}
	return "", false
}

type framePattern struct {
	re   *regexp.Regexp
	fn   int    // submatch holding the function name
	keep string // the frame without its line number
}

var framePatterns = []framePattern{
	// Node and browsers: "at NoticeList (app/notices/page.tsx:14:22)"
	{regexp.MustCompile(`\bat ((?:new |async )?\S+(?: \[as \S+\])?) \((.+?):\d+:\d+\)\s*$`), 1, "at $1 ($2)"},
	// Node, no function: "at /app/server.js:14:22" or "at node:internal/x:1:2"
	{regexp.MustCompile(`\bat ()\S+:\d+:\d+\s*$`), 1, ""},
	// Java and Kotlin: "at com.example.App.main(App.java:12)"
	{regexp.MustCompile(`\bat ([\w$.<>]+)\(([\w$.]+):\d+\)\s*$`), 1, "at $1($2)"},
	// Python: `File "app.py", line 12, in handler`
	{regexp.MustCompile(`\bFile "([^"]+)", line \d+, in (.+?)\s*$`), 2, `File "$1", in $2`},
	// Ruby: "from app/models/user.rb:12:in 'User#save'" (backticks before 3.4)
	{regexp.MustCompile("(\\S+\\.\\w+):\\d+:in [`']([^']+)'\\s*$"), 2, "$1:in '$2'"},
	// Elixir: "(my_app 0.1.0) lib/my_app/worker.ex:12: MyApp.Worker.run/1"
	{regexp.MustCompile(`\((\w+) [\w.\-+]+\) (\S+):\d+: (\S+)\s*$`), 3, "($1) $2: $3"},
}

var (
	// "main.main()", "net/http.(*conn).serve(0xc000123000, {0x6f0b20, 0xc0000a8000})",
	// "created by net/http.(*Server).Serve in goroutine 1". Go prints these at the
	// start of the line, which keeps indented source lines from other traces out.
	goFuncFrame = regexp.MustCompile(`^(?:created by [\w\-./]+(?:\.\(\*?[^)]+\))?\.[\w\-.\[\]]+(?: in goroutine \d+)?|[\w\-./]+(?:\.\(\*?[^)]+\))?\.[\w\-.\[\]]+\(.*\))\s*$`)
	// "\t/app/main.go:12 +0x1d"
	goFileFrame = regexp.MustCompile(`^(\s*\S+\.go):\d+(?: \+0x[0-9a-fA-F]+)?\s*$`)
)

// volatile is every part of a line that changes from one run of the same failure
// to the next, in the order they're replaced. Order matters: whole timestamps go
// before bare clock times, and IP addresses before ports.
var volatile = []struct {
	re   *regexp.Regexp
	with string
}{
	// Timestamps. ISO 8601 and the forms Rails, Python logging and most JSON
	// loggers print: "2026-01-01T09:00:00.123Z", "2026-01-01 09:00:00,123".
	{regexp.MustCompile(`\d{4}[-/]\d{2}[-/]\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:[.,]\d+)?)?(?:Z|[+-]\d{2}:?\d{2}|\b))?`), "<time>"},
	// Apache and nginx: "01/Jan/2026:09:00:00 +0000"
	{regexp.MustCompile(`\d{2}/[A-Z][a-z]{2}/\d{4}:\d{2}:\d{2}:\d{2}(?: [+-]\d{4})?`), "<time>"},
	// syslog: "Jan  1 09:00:00"
	{regexp.MustCompile(`\b(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2}\s+\d{1,2}:\d{2}:\d{2}\b`), "<time>"},
	// Bare clock times: "[09:00:01]", "09:00:01.123"
	{regexp.MustCompile(`\b\d{1,2}:\d{2}:\d{2}(?:[.,]\d+)?\b`), "<time>"},
	// Epoch seconds and milliseconds from 2017 to 2033.
	{regexp.MustCompile(`\b1[5-9]\d{8}(?:\d{3})?(?:\.\d+)?\b`), "<epoch>"},

	// Ids.
	{regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), "<uuid>"},
	{regexp.MustCompile(`(?i)\b((?:x-)?(?:request|req|trace|span|correlation)[_-]?id)(["']?\s*[=:]\s*["']?)[\w\-.]+`), "$1$2<id>"},
	{regexp.MustCompile(`0x[0-9a-fA-F]+`), "0x<hex>"},
	// Long runs of hex or digits with at least one digit: trace ids, request
	// ids, git shas, big counters. Words made of a to f alone are left alone.
	{regexp.MustCompile(`\b[a-fA-F]*\d[0-9a-fA-F]{7,}\b`), "<id>"},

	// Where it's running.
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`), "<ip>"},
	{regexp.MustCompile(`(localhost|<ip>|://[^/\s:]+|\]|::):\d{1,5}\b`), "$1:<port>"},
	{regexp.MustCompile(`(?i)\b(port)(\s*[:=]?\s*)\d{1,5}\b`), "$1$2<port>"},
	{regexp.MustCompile(`(?i)\b(pid)(\s*[:=#]?\s*)\d+\b`), "$1$2<pid>"},
	// Ruby's Logger: "E, [2026-01-01T09:00:00.123456 #12345] ERROR"
	{regexp.MustCompile(`(<time> #)\d+\]`), "$1<pid>]"},
	// syslog, after the time and host: "Jan  1 09:00:00 box node[12345]: "
	{regexp.MustCompile(`^(<time> \S+ [\w.\-/]+)\[\d+\]:`), "$1[<pid>]:"},
	// puma at the start of a line: "[12345] ! Unable to load application"
	{regexp.MustCompile(`^(\s*)\[\d+\] `), "$1[<pid>] "},
	{regexp.MustCompile(`\bgoroutine \d+\b`), "goroutine <n>"},

	// How long and how often.
	{regexp.MustCompile(`\b(?:\d+(?:\.\d+)?(?:ns|µs|us|ms|h|m|s))+\b`), "<duration>"},
	{regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s?(?:seconds?|secs?|minutes?|mins?|hours?|milliseconds?)\b`), "<duration>"},
	{regexp.MustCompile(`(?i)\b\d+ (retries|retry|attempts?|tries|restarts?)\b`), "<n> $1"},
	{regexp.MustCompile(`(?i)\b(attempt|retry|try|restart)(\s*#?)\d+(?:\s*(?:of|/)\s*\d+)?`), "$1$2<n>"},
}

// tmpPath matches a path under a temp directory, at the start of a path rather
// than inside a project's own tmp directory. The directory is random, but the file
// name usually says what it is, so tmpBase keeps it when it can.
var tmpPath = regexp.MustCompile(`(^|[\s'"(=:])((?:/private)?/var/folders/[^\s:'"()]+|/tmp/[^\s:'"()]+|[A-Za-z]:\\[^\s:'"()]*\\Temp\\[^\s:'"()]+)`)

// normalise swaps the volatile parts of a line for placeholders and evens out
// the spacing, so two copies of the same error read the same.
func normalise(l string) string {
	l = cut(l, maxScannedLine)
	l = tmpPath.ReplaceAllStringFunc(l, func(m string) string {
		sub := tmpPath.FindStringSubmatch(m)
		return sub[1] + "<tmp>/" + tmpBase(sub[2])
	})
	for _, v := range volatile {
		l = v.re.ReplaceAllString(l, v.with)
	}
	return cut(strings.Join(strings.Fields(l), " "), maxFingerprintLine)
}

// tmpExt is a file extension that looks chosen rather than random.
var tmpExt = regexp.MustCompile(`^\.[a-z]{1,5}$`)

// tmpBase is the last element of a temp path, or a placeholder if it looks random.
// Temp names often are ("tmp.Xy12ab", "go-build1234", "tmpk3j_x9.json"), so the
// name is kept only with a plain extension, no digits and no "tmp" at the start,
// which is about as far as a guess should go.
func tmpBase(p string) string {
	base := path.Base(strings.ReplaceAll(p, `\`, "/"))
	if !tmpExt.MatchString(path.Ext(base)) || strings.ContainsAny(base, "0123456789") || strings.HasPrefix(base, "tmp") {
		return "<file>"
	}
	return base
}

// cut shortens l to at most n bytes without splitting a character. It backs off
// no further than one character's worth, so bytes that aren't valid UTF-8 can't
// make it throw the whole line away.
func cut(l string, n int) string {
	if len(l) <= n {
		return l
	}
	i := n
	for i > n-utf8.UTFMax && !utf8.RuneStart(l[i]) {
		i--
	}
	if !utf8.RuneStart(l[i]) {
		i = n
	}
	return l[:i]
}

// group files a caught error: as a repeat of an entry already in the feed, or as
// a new one.
//
// This is what calms a crash loop. A repeat of an open entry (new, sent or still
// failing) always adds to it, however long it's been, so a server stuck restarting
// is one entry with a rising count and nothing else.
//
// A repeat of a dismissed entry within Cooldown of the last time it was seen is
// counted but stays dismissed, and the cooldown starts again from this repeat. So
// a dismissed crash loop stays out of the way for as long as it keeps looping.
// Once it has been quiet for longer than Cooldown, the loop is taken to have
// stopped, and the next time the error appears it's a fresh entry.
//
// A repeat of a finished entry (fixed or unverified) is always a new entry: the
// error came back after a verdict, which is news. So is a repeat that came too
// late to count toward a settle period (see pastSettle).
func (e *Engine) group(src Source, f found) {
	key := groupKey{src.ID, fingerprint(f)}
	if r := e.repeatOf(key, f); r != nil {
		r.entry.Count++
		if f.matchedAt.After(r.entry.LastSeen) {
			r.entry.LastSeen = f.matchedAt
		}
		// The context is the latest occurrence, so the source details must be too.
		r.entry.Source = src
		r.entry.Lines = f.lines
		r.entry.Before = f.before
		r.entry.After = f.after
		r.entry.MaybeGap = f.maybeGap
		e.recurred(r, f.matchedAt)
		return
	}
	r := e.add(Entry{
		Source:      src,
		Matcher:     f.matcher,
		Fingerprint: key.fingerprint,
		Lines:       f.lines,
		Before:      f.before,
		After:       f.after,
		Count:       1,
		FirstSeen:   f.matchedAt,
		LastSeen:    f.matchedAt,
		State:       StateNew,
		MaybeGap:    f.maybeGap,
	})
	if e.groups.newest == nil {
		e.groups.newest = make(map[groupKey]*record)
	}
	e.groups.newest[keyOf(r)] = r
}

// repeatOf finds the entry f should be counted against, or nil.
func (e *Engine) repeatOf(key groupKey, f found) *record {
	r, ok := e.groups.newest[key]
	if !ok {
		return nil
	}
	if r.entry.State.open() {
		if e.pastSettle(r, f.matchedAt) {
			return nil
		}
		return r
	}
	if r.entry.State == StateDismissed && f.matchedAt.Sub(r.entry.LastSeen) <= e.opts.Cooldown {
		return r
	}
	return nil
}

// Output is untrusted, so the number of entries is capped (#41). A dev server
// printing a slightly different error for every request would otherwise grow the
// feed without limit.
//
// When there are more than MaxEntries, entries go in this order, each group
// oldest first by LastSeen:
//
//  1. dismissed, because I've already said I don't care
//  2. finished (fixed or unverified), because the verdict is in
//  3. new, which I haven't acted on yet
//
// An entry that's being checked is never dropped. That's one that has been sent
// (whether its agent is still working or its settle period has started) or one
// that's still failing after a send. Dropping either would throw away the answer
// to "did the fix work?", which is the point of Sightline. Only a keypress can
// make an entry sent, so output can't fill the feed with them. If every entry
// left is being checked, the feed goes over the cap rather than drop one. It's
// only trimmed when a new entry arrives, so it can stay over the cap after those
// checks finish, until the next new error. That's bounded by keypresses too.
//
// The entry just added is never dropped either, so a new error always reaches
// the feed. It's the newest, so it would be last in its group anyway.
const (
	dropDismissed = iota
	dropFinished
	dropNew
	neverDrop
)

// dropOrder says which group r is dropped with.
func dropOrder(r *record) int {
	switch r.entry.State {
	case StateDismissed:
		return dropDismissed
	case StateFixed, StateUnverified:
		return dropFinished
	case StateNew:
		return dropNew
	}
	return neverDrop
}

// trim drops entries until there are no more than MaxEntries, keeping just.
//
// Finding the next entry to drop is one walk over the entries, so it's about
// one walk for each new entry and nothing for a repeat.
func (e *Engine) trim(just *record) {
	for len(e.records) > e.opts.MaxEntries {
		i := e.nextToDrop(just)
		if i < 0 {
			return
		}
		e.drop(i)
	}
}

// nextToDrop is the index in records of the entry to drop next, or -1 if there's
// nothing that may be dropped. Records are oldest first, so on a tie the entry
// caught first goes first.
func (e *Engine) nextToDrop(just *record) int {
	best, bestOrder := -1, neverDrop
	for i, r := range e.records {
		o := dropOrder(r)
		if o == neverDrop || r == just {
			continue
		}
		if best < 0 || o < bestOrder || (o == bestOrder && r.entry.LastSeen.Before(e.records[best].entry.LastSeen)) {
			best, bestOrder = i, o
		}
	}
	return best
}

// drop forgets the entry at records[i]. The grouping index only loses its key
// if the key still points at this entry, because a newer entry for the same
// failure may have taken it over. A later repeat of a dropped error is a fresh
// entry.
func (e *Engine) drop(i int) {
	r := e.records[i]
	e.records = slices.Delete(e.records, i, i+1)
	delete(e.byID, r.entry.ID)
	if k := keyOf(r); e.groups.newest[k] == r {
		delete(e.groups.newest, k)
	}
	e.dropped++
}
