package config

import (
	"bytes"
	"errors"
	"fmt"
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
	"Uncaught Error: ", "Uncaught TypeError: ", "UnhandledPromiseRejection: ",
	"ValueError: ", "KeyError: ", "AttributeError: ", "ModuleNotFoundError: ",
	"error: ", "ERROR: ", "ERROR ", "FATAL ", "fatal: ", "fatal error: ",
	"panic: ", "panic: runtime error: ", "thread 'main' panicked at ",
	"error[E0425]: ", "FAIL ", "FAILED ",
	`Exception in thread "main" java.lang.RuntimeException: `,
	"NoMethodError (undefined method ", "ActionView::Template::Error (",
}

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
	return append(probes,
		"Traceback (most recent call last):",
		"fatal error: all goroutines are asleep - deadlock!",
		"goroutine 1 [running]:",
	)
}()

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

// short quotes a regex for an error message, cut so one long rule can't take
// over the line.
func short(expr string) string {
	const most = 80
	if len(expr) <= most {
		return strconv.Quote(expr)
	}
	cut := most
	for cut > 0 && !utf8.RuneStart(expr[cut]) {
		cut--
	}
	return strconv.Quote(expr[:cut]) + "..."
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
