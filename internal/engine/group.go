package engine

import (
	"path"
	"regexp"
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

// maxFingerprintLine bounds how much of each line is fingerprinted. Output is
// untrusted and a line can be any length. Lines that agree for this long are the
// same error for grouping's purposes.
const maxFingerprintLine = 1024

// fingerprint says which failure f is. Two errors with the same fingerprint from
// the same source are the same failure. It's also what the fix check (#12) uses to
// tell the same error coming back from a different one.
func fingerprint(f found) string {
	if len(f.lines) == 0 {
		return ""
	}
	first := clip(f.lines[0])
	rest := append(append([]string(nil), f.lines[1:]...), f.after...)
	parts := append([]string{normalise(first)}, frames(rest, pythonTraceback.MatchString(first))...)
	return strings.Join(parts, "\n")
}

var (
	// pythonTraceback starts a Python traceback. Python prints the innermost frame
	// last and the exception after it, so a traceback is read differently.
	pythonTraceback = regexp.MustCompile(`Traceback \(most recent call last\):\s*$`)
	// pythonException is the line that ends a traceback: "ValueError: bad input".
	pythonException = regexp.MustCompile(`^[A-Za-z_][\w.]*(?:Error|Exception|Exit|Interrupt|Warning)\b`)
)

// frames picks out the stack frames from the lines after an error, normalised.
// Lines that aren't frames are skipped rather than ending the search, because
// traces mix frames with source lines (Python) and headers (Go).
//
// Usually that's the first few frames, nearest the error. In a Python traceback
// it's the last few, which are the nearest, plus the exception line that ends it,
// because the line that matched may only have been the traceback's header.
func frames(lines []string, python bool) []string {
	var out []string
	n := 0 // frames started, as a Go frame takes two lines
	afterGoFunc := false
	for _, l := range lines {
		l = clip(l)
		if pythonTraceback.MatchString(l) {
			python = true
			continue
		}
		if python && pythonException.MatchString(l) {
			return append(last(out, maxFrames), normalise(l))
		}
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
		if !ok {
			continue
		}
		if !python && n == maxFrames {
			break
		}
		out = append(out, normalise(f))
		n++
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
	// ids, git shas, big counters. Words made of a to f alone are left be.
	{regexp.MustCompile(`\b[a-fA-F]*\d[0-9a-fA-F]{7,}\b`), "<id>"},

	// Where it's running.
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`), "<ip>"},
	{regexp.MustCompile(`(localhost|<ip>|://[^/\s:]+|\]|::):\d{1,5}\b`), "$1:<port>"},
	{regexp.MustCompile(`(?i)\b(port)(\s*[:=]?\s*)\d{1,5}\b`), "$1$2<port>"},
	{regexp.MustCompile(`(?i)\b(pid)(\s*[:=#]?\s*)\d+\b`), "$1$2<pid>"},
	{regexp.MustCompile(`#\d+\]`), "#<pid>]"},                // Ruby's Logger: "[2026-01-01T09:00:00 #12345]"
	{regexp.MustCompile(`(\w)\[\d+\]:`), "$1[<pid>]:"},       // syslog: "node[12345]:"
	{regexp.MustCompile(`(^\s*|\| )\[\d+\] `), "$1[<pid>] "}, // puma, alone or under docker compose: "[12345] ! Unable to load application"
	{regexp.MustCompile(`\bgoroutine \d+\b`), "goroutine <n>"},

	// How long and how often.
	{regexp.MustCompile(`\b(?:\d+(?:\.\d+)?(?:ns|µs|us|ms|h|m|s))+\b`), "<duration>"},
	{regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s?(?:seconds?|secs?|minutes?|mins?|hours?|milliseconds?)\b`), "<duration>"},
	{regexp.MustCompile(`(?i)\b\d+ (retries|retry|attempts?|tries|times|restarts?)\b`), "<n> $1"},
	{regexp.MustCompile(`(?i)\b(attempt|retry|try|restart)(\s*#?)\d+(?:\s*(?:of|/)\s*\d+)?`), "$1$2<n>"},
}

// tmpPath matches a path under a temp directory. The directory is random, but the
// file name usually says what it is, so tmpBase keeps it when it can.
var tmpPath = regexp.MustCompile(`(?:/private)?/var/folders/[^\s:'"()]+|/tmp/[^\s:'"()]+|\b[A-Za-z]:\\[^\s:'"()]*\\Temp\\[^\s:'"()]+`)

// normalise swaps the volatile parts of a line for placeholders and evens out
// the spacing, so two copies of the same error read the same.
func normalise(l string) string {
	l = clip(l)
	l = tmpPath.ReplaceAllStringFunc(l, func(p string) string {
		return "<tmp>/" + tmpBase(p)
	})
	for _, v := range volatile {
		l = v.re.ReplaceAllString(l, v.with)
	}
	return strings.Join(strings.Fields(l), " ")
}

// tmpBase is the last element of a temp path, minus anything that looks random.
func tmpBase(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	base := path.Base(p)
	// Temp file names are often random too ("tmp.Xy12ab", "go-build1234"). Keep
	// the name only if it has an extension and no digits before it, which is
	// about as far as a guess should go.
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if ext == "" || strings.ContainsAny(stem, "0123456789") {
		return "<file>"
	}
	return base
}

// clip cuts l to maxFingerprintLine bytes without splitting a character.
func clip(l string) string {
	if len(l) <= maxFingerprintLine {
		return l
	}
	i := maxFingerprintLine
	for i > 0 && !utf8.RuneStart(l[i]) {
		i--
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
