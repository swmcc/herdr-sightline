package config

import (
	"bytes"
	"errors"
	"fmt"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

// ProjectFileName is the project file a repo can carry at its root (#22).
const ProjectFileName = ".sightline.toml"

// A project file is written by whoever can push to the repo, so it's held to
// tighter limits than the person's own file.
const (
	// MaxProjectFileSize is the most of a project file Sightline will read.
	MaxProjectFileSize = 64 << 10
	// MaxProjectRules is the most patterns, and separately the most ignore
	// rules, one project file can add.
	MaxProjectRules = 50
	// MaxProjectRegex is the longest regex a project file can hold, in bytes.
	// A rule is meant to be read before it's trusted, and a long one can push
	// what matters off the screen.
	MaxProjectRegex = 500
	// MaxProjectName is the longest pattern name a project file can hold.
	MaxProjectName = 64
)

// Project is what a trusted project file adds to the person's own config. It
// only ever adds. Merging it is the caller's job, and the person's own
// sightline.toml wins on every conflict.
type Project struct {
	// Presets are built-in pattern sets to switch on. PresetsSet says the file
	// listed them at all, which an empty list also counts as. Unlike the
	// person's own list, a project's can only switch presets on, never off, so
	// an empty one does nothing.
	Presets    []string
	PresetsSet bool
	// Patterns are error patterns, on top of the person's own.
	Patterns []Pattern
	// Ignores are ignore rules, on top of the person's own. They're the risky
	// part: a rule from a repo can hide a real error.
	Ignores []Ignore
}

// projectKeys are the only keys a project file may set, with the keys each
// table may hold. It's an allowlist, so a setting added to sightline.toml later
// can't be set from a repo until it's added here on purpose. Anything that
// decides what's sent, to which agent or when (settle_seconds, context_lines,
// [[watch]] pins, the closing line, queue mode) stays out.
var projectKeys = map[string][]string{
	"presets": nil,
	"pattern": {"name", "regex"},
	"ignore":  {"regex"},
}

// errorHeads are how the usual errors in each preset's stack start. A
// project's ignore rule must not match one of them followed by any message,
// or it would hide a whole kind of error in that repo.
var errorHeads = []string{
	"Error: ", "TypeError: ", "SyntaxError: ", "ReferenceError: ", "RangeError: ",
	"Uncaught Error: ", "Uncaught TypeError: ", "[UnhandledPromiseRejection: ",
	"UnhandledPromiseRejectionWarning: ",
	"ValueError: ", "KeyError: ", "AttributeError: ", "ModuleNotFoundError: ",
	"IndexError: ", "RuntimeError: ", "AssertionError: ", "ZeroDivisionError: ",
	"NameError: ", "ImportError: ", "OSError: ", "FileNotFoundError: ",
	"PermissionError: ", "ConnectionError: ", "TimeoutError: ",
	"error: ", "ERROR: ", "ERROR ", "ERROR in ", "FATAL ", "fatal: ", "fatal error: ",
	"panic: ", "panic: runtime error: ", "thread 'main' panicked at ",
	"error[E0425]: ", "error TS2304: ", "src/main.c:1:1: error: ",
	"FAIL ", " FAIL ", "FAILED ", "--- FAIL: ", "test result: FAILED. ",
	`Exception in thread "main" java.lang.RuntimeException: `,
	"java.lang.IllegalStateException: ", "org.example.AppException: ",
	"NoMethodError (undefined method ", "ActionView::Template::Error (",
}

// labels are the process manager labels the presets match through (see
// labelPrefix in the engine): docker compose, foreman and turborepo. Every
// probe is tried bare and behind each one.
var labels = []string{"", "web-1  | ", "12:00:00 web.1  | ", "web:dev: "}

// projectErrors are the lines an ignore rule from a project is tried against:
// each head followed by messages that start with every printable character,
// so a rule that only leaves out a few first letters (^Error: [^bC]) still
// matches one, plus a few whole lines that have no message to vary. It catches
// the obvious ways to hide a kind of error, not every one. The trust view and
// the count of what was hidden are the real defence.
var projectErrors = func() []string {
	probes := CommonErrors()
	for _, head := range errorHeads {
		for c := byte('!'); c <= '~'; c++ {
			probes = append(probes, head+string(c)+"x failed")
		}
		probes = append(probes, head+"boom", head+"Cannot find module 'x'", head+" x")
	}
	probes = append(probes,
		"Traceback (most recent call last):",
		"fatal error: all goroutines are asleep - deadlock!",
		"goroutine 1 [running]:",
		"Segmentation fault (core dumped)",
		"bash: foo: command not found",
		"=================================== FAILURES ===================================",
		"  3 failing",
	)
	var all []string
	for _, label := range labels {
		for _, p := range probes {
			all = append(all, label+p)
		}
	}
	return all
}()

// minIgnoreHead is the shortest fixed text a project's ignore rule can start
// with, and minPastHead how much of it must come after a known error head.
const (
	minIgnoreHead = 12
	minPastHead   = 4
)

// ignoreHead is the fixed text every match of expr starts with, after the ^,
// and whether there is a ^. The text is "" when there's none: the rule starts
// with a class, a repeat, an alternation or a case insensitive part.
func ignoreHead(expr string) (head string, anchored bool) {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return "", false
	}
	re = re.Simplify()
	subs := []*syntax.Regexp{re}
	if re.Op == syntax.OpConcat {
		subs = re.Sub
	}
	var b strings.Builder
	for i, s := range subs {
		switch {
		case i == 0 && (s.Op == syntax.OpBeginLine || s.Op == syntax.OpBeginText):
			anchored = true
		case s.Op == syntax.OpLiteral && s.Flags&syntax.FoldCase == 0:
			b.WriteString(string(s.Rune))
		default:
			return b.String(), anchored
		}
	}
	return b.String(), anchored
}

// tooLoose says why a project's ignore rule doesn't name what it hides
// narrowly enough, or "" when it does. Probes can only ever try some of the
// lines a rule could hide, so on top of them a project's ignore rule has to:
//
//   - start with ^, so it only matches lines that start with its text. A rule
//     that matches anywhere ("is not defined") hides every error that says it.
//   - go on with at least minIgnoreHead bytes of fixed text, with no class,
//     alternation or (?i) in them, which is where a rule hides a whole kind
//     of error.
//   - name an error and its message: a known error head or a ": ", followed
//     by at least minPastHead characters of the message. A label or a word on
//     its own (^my-frontend-1 | ) could hide everything a process prints.
//
// It costs some rules that would be fine, but those can go in the person's
// own sightline.toml, which has no such limits.
func tooLoose(expr string) string {
	head, anchored := ignoreHead(expr)
	if !anchored {
		return "has to start with ^. A rule that matches anywhere in a line could hide any error that mentions those words"
	}
	if len(head) < minIgnoreHead {
		return fmt.Sprintf("has to start with ^ and at least %d characters of fixed text, with no class, alternation or (?i) in them, so it's plain what it hides", minIgnoreHead)
	}
	named := fmt.Sprintf("has to name the message, with at least %d characters of fixed text after the error's own start, so it hides one error and not a kind of them", minPastHead)
	known := false
	for _, h := range errorHeads {
		h = strings.TrimSpace(h)
		if strings.HasPrefix(head, h) {
			known = true
			if len(strings.TrimSpace(head[len(h):])) < minPastHead {
				return named
			}
		}
	}
	if i := strings.LastIndex(head, ": "); i >= 0 {
		if len(strings.TrimSpace(head[i+2:])) < minPastHead {
			return named
		}
	} else if !known {
		return "has to start with an error and its message with \": \" between them, like ^Error: flaky upstream, so it's plain which error it hides"
	}
	return ""
}

// ParseProject reads a project file. name is only used in error messages.
//
// The file is refused whole, never applied in part, when it sets anything but
// presets, [[pattern]] and [[ignore]], is too big, has too many rules or one
// that's too long, or has a rule that would match far too much: a pattern that
// matches an empty line or ordinary output, or an ignore rule that does, or that
// matches a common error. Half a file someone else wrote is harder to reason
// about than none of it.
func ParseProject(name string, data []byte) (Project, error) {
	if len(data) > MaxProjectFileSize {
		return Project{}, &Error{File: name, Msg: fmt.Sprintf("file is bigger than %d KiB, which is far too big for a project file", MaxProjectFileSize>>10)}
	}
	data = bytes.TrimPrefix(data, utf8BOM)

	var keys map[string]any
	mapErr := toml.Unmarshal(data, &keys)
	if mapErr == nil {
		if errs := disallowed(name, data, keys); len(errs) > 0 {
			sortErrors(errs)
			return Project{}, errors.Join(errs...)
		}
		// Parse compiles every regex, so the sizes are checked first. A short
		// regex can still cost a lot to compile ([ab]{1000} a hundred times).
		if errs := oversized(name, data, keys); len(errs) > 0 {
			sortErrors(errs)
			return Project{}, errors.Join(errs...)
		}
	}
	// A syntax error is left to Parse, which says it far better.
	cfg, err := Parse(name, data)
	if err != nil {
		return Project{}, err
	}
	if mapErr != nil {
		// The keys weren't checked, so it isn't used, however Parse got on.
		return Project{}, &Error{File: name, Msg: fmt.Sprintf("couldn't check which keys it sets: %v", mapErr)}
	}

	p := Project{Patterns: cfg.Patterns, Ignores: cfg.Ignores}
	if _, ok := keys["presets"]; ok {
		p.Presets, p.PresetsSet = cfg.Presets, true
	}

	w := walkDocument(data)
	v := validator{name: name, positions: w.positions}
	if n := len(p.Patterns); n > MaxProjectRules {
		v.fail("pattern", "a project file can have at most %d [[pattern]] tables, this one has %d", MaxProjectRules, n)
	}
	if n := len(p.Ignores); n > MaxProjectRules {
		v.fail("ignore", "a project file can have at most %d [[ignore]] tables, this one has %d", MaxProjectRules, n)
	}
	for i, pat := range p.Patterns {
		key := "pattern." + strconv.Itoa(i) + ".regex"
		expr := pat.Regex.String()
		switch line, broad := MatchesOrdinary(pat.Regex); {
		case len(pat.Name) > MaxProjectName || strings.ContainsFunc(pat.Name, unreadable):
			v.fail("pattern."+strconv.Itoa(i)+".name", "[[pattern]] %s needs a name of at most %d plain characters", short(pat.Name), MaxProjectName)
		case len(expr) > MaxProjectRegex:
			v.fail(key, "[[pattern]] %q has a regex over %d bytes, which is too long to read before trusting it", pat.Name, MaxProjectRegex)
		case padded(expr):
			v.fail(key, "[[pattern]] %q has a run of spaces in its regex, which could hide part of it on screen. Use ' {2}' or \\s+", pat.Name)
		case broad && line == "":
			v.fail(key, "[[pattern]] %q matches an empty line, so every line would be an error", pat.Name)
		case broad:
			v.fail(key, "[[pattern]] %q matches ordinary output like %q, so it would cry wolf", pat.Name, line)
		}
	}
	for i, ig := range p.Ignores {
		key := "ignore." + strconv.Itoa(i) + ".regex"
		expr := ig.Regex.String()
		line, broad := MatchesOrdinary(ig.Regex)
		hides := slices.IndexFunc(projectErrors, ig.Regex.MatchString)
		switch {
		case len(expr) > MaxProjectRegex:
			v.fail(key, "[[ignore]] has a regex over %d bytes, which is too long to read before trusting it", MaxProjectRegex)
		case padded(expr):
			v.fail(key, "[[ignore]] %s has a run of spaces, which could hide part of it on screen. Use ' {2}' or \\s+", short(expr))
		case broad && line == "":
			v.fail(key, "[[ignore]] %s matches an empty line, so it would hide everything", short(expr))
		case broad:
			v.fail(key, "[[ignore]] %s matches ordinary output like %q, so it would hide far too much", short(expr), line)
		case hides >= 0:
			v.fail(key, "[[ignore]] %s matches common errors like %q, so it could hide real ones", short(expr), projectErrors[hides])
		default:
			if why := tooLoose(expr); why != "" {
				v.fail(key, "[[ignore]] %s %s", short(expr), why)
			}
		}
	}
	if len(v.errs) > 0 {
		sortErrors(v.errs)
		return Project{}, errors.Join(v.errs...)
	}
	return p, nil
}

// padded reports whether a regex has two or more whitespace characters in a
// row, or any whitespace but a space. Either can push the rest of a rule out
// of sight when it's shown before being trusted, and a real rule can always
// say ' {2}' or \s instead.
func padded(expr string) bool {
	return strings.Contains(expr, "  ") || strings.ContainsFunc(expr, func(r rune) bool {
		return r != ' ' && unicode.IsSpace(r)
	})
}

// unreadable reports whether r doesn't show as itself on a terminal.
func unreadable(r rune) bool {
	return r == utf8.RuneError || !unicode.IsPrint(r) || unicode.Is(unicode.Cf, r)
}

// maxRegexCost is the most a project regex can cost to compile, roughly in
// instructions: repeats count as many times as they repeat.
const maxRegexCost = 5000

// oversized checks the counts and sizes of a project file's rules before
// anything is compiled.
func oversized(name string, data []byte, keys map[string]any) []error {
	w := walkDocument(data)
	v := validator{name: name, positions: w.positions}
	for _, table := range []string{"pattern", "ignore"} {
		rows, _ := keys[table].([]any)
		if len(rows) > MaxProjectRules {
			v.fail(table+".0", "a project file can have at most %d [[%s]] tables, this one has %d", MaxProjectRules, table, len(rows))
			continue
		}
		for i, row := range rows {
			fields, _ := row.(map[string]any)
			expr, _ := fields["regex"].(string)
			key := table + "." + strconv.Itoa(i) + ".regex"
			switch {
			case len(expr) > MaxProjectRegex:
				v.fail(key, "[[%s]] has a regex over %d bytes, which is too long to read before trusting it", table, MaxProjectRegex)
			case regexCost(expr) > maxRegexCost:
				v.fail(key, "[[%s]] %s repeats too much, which would take far too much memory to compile", table, short(expr))
			}
		}
	}
	return v.errs
}

// regexCost estimates what compiling expr costs, without compiling it: each
// part counts once, and a repeat counts its part as many times as it can
// repeat. A regex that doesn't parse costs nothing here, and Parse reports it.
func regexCost(expr string) int {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return 0
	}
	var cost func(*syntax.Regexp) int
	cost = func(re *syntax.Regexp) int {
		n := 1 + len(re.Rune)
		for _, sub := range re.Sub {
			n += cost(sub)
		}
		if re.Op == syntax.OpRepeat {
			times := max(re.Min, re.Max, 1)
			n *= times
		}
		return min(n, maxRegexCost+1)
	}
	return cost(re)
}

// short quotes a regex for an error message, cut so one long rule can't take
// over the line.
func short(expr string) string {
	const most = 80
	if len(expr) <= most {
		return strconv.QuoteToASCII(expr)
	}
	cut := most
	for cut > 0 && !utf8.RuneStart(expr[cut]) {
		cut--
	}
	return strconv.QuoteToASCII(expr[:cut]) + "..."
}

// disallowed lists every key the file sets that a project file can't. A key
// that only differs in case from an allowed one is left to Parse, which says
// it's spelt wrong.
func disallowed(name string, data []byte, keys map[string]any) []error {
	w := walkDocument(data)
	v := validator{name: name, positions: w.positions}
	allowed := func(set []string, key string) bool {
		return slices.ContainsFunc(set, func(k string) bool { return strings.EqualFold(k, key) })
	}
	top := make([]string, 0, len(projectKeys))
	for k := range projectKeys {
		top = append(top, k)
	}
	for _, k := range sortedKeys(keys) {
		if !allowed(top, k) {
			at := k
			if _, ok := v.positions[at]; !ok {
				// An array of tables is placed by its first header.
				at = k + ".0"
			}
			v.fail(at, "%s can't be set from a project file. Only presets, [[pattern]] and [[ignore]] can, so it's up to you in your own sightline.toml", k)
			continue
		}
		tables, ok := keys[k].([]any)
		if !ok {
			continue
		}
		for i, t := range tables {
			table, ok := t.(map[string]any)
			if !ok {
				continue
			}
			for _, field := range sortedKeys(table) {
				if !allowed(projectKeys[strings.ToLower(k)], field) {
					key := k + "." + strconv.Itoa(i) + "." + field
					v.fail(key, "%s can't be set in a project file's [[%s]]", field, strings.ToLower(k))
				}
			}
		}
	}
	return v.errs
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
