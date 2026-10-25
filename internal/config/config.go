// Package config reads sightline.toml.
//
// It's a plain loader. It turns the file into a Config and says clearly what's wrong
// when it can't. It knows nothing about herdr, and the engine doesn't know about it
// either. Whoever owns both translates a Config into engine inputs.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// FileName is the name of the config file inside the plugin's config directory.
const FileName = "sightline.toml"

// Defaults from the README. Every setting is optional.
const (
	DefaultSettleSeconds = 30
	DefaultContextLines  = 20
)

// presetNames are the built-in pattern sets, all on by default.
var presetNames = []string{"generic", "node", "python", "ruby", "go", "rust", "jvm", "tests"}

// Presets returns the names of every built-in pattern set.
func Presets() []string {
	return slices.Clone(presetNames)
}

// Config is everything sightline.toml can set.
type Config struct {
	// SettleSeconds is how long to watch after a fix before calling it.
	SettleSeconds int
	// ContextLines is how many lines of output to send before and after the error.
	ContextLines int
	// Presets are the built-in pattern sets that are switched on.
	Presets []string
	// Patterns are the user's own error patterns.
	Patterns []Pattern
	// Ignores are lines never to alert on, even when a pattern matches.
	Ignores []Ignore
	// Pins send errors from a pane with a given title to a named agent.
	Pins []Pin
}

// Pattern is a custom error pattern from a [[pattern]] table.
type Pattern struct {
	Name  string
	Regex *regexp.Regexp
}

// Ignore is a rule from an [[ignore]] table.
type Ignore struct {
	Regex *regexp.Regexp
}

// Pin is a [[watch]] table. It ties a pane title to an agent.
type Pin struct {
	PaneTitle string
	Agent     string
}

// Default is the config used when there's no file at all.
func Default() Config {
	return Config{
		SettleSeconds: DefaultSettleSeconds,
		ContextLines:  DefaultContextLines,
		Presets:       Presets(),
	}
}

// Path is where the config file lives inside dir. dir is normally
// HERDR_PLUGIN_CONFIG_DIR, the same path `herdr plugin config-dir swmcc.sightline`
// prints.
func Path(dir string) string {
	return filepath.Join(dir, FileName)
}

// Limits on the numbers. Anything past these is a mistake, and letting it through
// would overflow a time.Duration or size a buffer to take the feed down later.
const (
	MaxSettleSeconds = 3600
	MaxContextLines  = 1000
)

// MaxFileSize is the most Sightline will read. A real config is a few hundred bytes.
const MaxFileSize = 1 << 20

// arrayTables are the keys that must be arrays of tables, written [[name]].
var arrayTables = []string{"pattern", "ignore", "watch"}

// utf8BOM is stripped before decoding. Some editors on Windows add it.
var utf8BOM = []byte("\xEF\xBB\xBF")

// Error is one problem with the file. Line and Column start at 1 and are 0 when
// the position isn't known.
type Error struct {
	File   string
	Line   int
	Column int
	Msg    string
}

// Error keeps each problem on one line. A regex from a multi-line string can put
// newlines in the message, and the feed shows one error per line.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.File)
	if e.Line > 0 {
		b.WriteString(":" + strconv.Itoa(e.Line))
		if e.Column > 0 {
			b.WriteString(":" + strconv.Itoa(e.Column))
		}
	}
	b.WriteString(": " + oneLine.Replace(e.Msg))
	return b.String()
}

var oneLine = strings.NewReplacer("\r", `\r`, "\n", `\n`)

// Load reads the config file from dir. A missing file isn't an error, it means the
// defaults. Problems with the file come back as one or more *Error values joined
// together, ordered by line.
func Load(dir string) (Config, error) {
	path := Path(dir)
	data, exists, err := readFile(path)
	if err != nil {
		return Config{}, err
	}
	if !exists {
		return Default(), nil
	}
	return Parse(path, data)
}

// readFile reads the config file, refusing anything over MaxFileSize.
func readFile(path string) (data []byte, exists bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading config: %w", err)
	}
	defer f.Close()
	data, err = io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, true, fmt.Errorf("reading config: %w", err)
	}
	if len(data) > MaxFileSize {
		return nil, true, &Error{File: path, Msg: "file is bigger than 1 MiB, which is far too big for a config file"}
	}
	return data, true, nil
}

// Parse turns the contents of a config file into a Config. name is only used in
// error messages.
//
// Most problems are collected and reported together. Unknown keys don't stop the
// rest of the file being checked, because the decoder has filled in everything it
// did recognise. A syntax error or a value of the wrong type does stop it, because
// then the decoded values can't be trusted, so that's the only error you'll see.
// A key in the wrong case stops the value checks too. TOML keys are case
// sensitive but the decoder isn't, so it has already taken the value under the
// right name and any problem with it would have no line to point at.
func Parse(name string, data []byte) (Config, error) {
	data = bytes.TrimPrefix(data, utf8BOM)

	// The walk stops quietly at a syntax error, which the decoder reports properly
	// below. Whatever positions it found up to there are still useful.
	w := walkDocument(data)
	v := validator{name: name, positions: w.positions}
	for _, s := range w.shapeErrors {
		v.fail(s.key, "%s", s.msg)
	}
	for _, c := range w.caseErrors {
		v.errs = append(v.errs, &Error{File: name, Line: c.pos.Line, Column: c.pos.Column, Msg: c.msg})
	}

	var raw fileConfig
	dec := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	var errs []error
	if err := dec.Decode(&raw); err != nil {
		var strict *toml.StrictMissingError
		if !errors.As(err, &strict) {
			// A table written the wrong way, like ignore = { ... }, also makes the
			// decoder give up. When that's the cause, the layout error says what to
			// do about it far better than the decoder can. Otherwise the decoder's
			// error is the real one, and any layout errors go alongside it.
			if w.causedFailure(err) {
				sortErrors(v.errs)
				return Config{}, errors.Join(v.errs...)
			}
			errs := append(v.errs, decodeError(name, err))
			sortErrors(errs)
			return Config{}, errors.Join(errs...)
		}
		errs = append(errs, unknownKeyErrors(name, strict)...)
	}
	if len(w.caseErrors) > 0 {
		errs = append(errs, v.errs...)
		sortErrors(errs)
		return Config{}, errors.Join(errs...)
	}
	cfg := v.build(raw)
	errs = append(errs, v.errs...)
	if len(errs) > 0 {
		sortErrors(errs)
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// sortErrors orders errors by where they are in the file. Ones without a position
// go last.
func sortErrors(errs []error) {
	pos := func(err error) (int, int) {
		var e *Error
		if errors.As(err, &e) && e.Line > 0 {
			return e.Line, e.Column
		}
		return int(^uint(0) >> 1), 0
	}
	slices.SortStableFunc(errs, func(a, b error) int {
		al, ac := pos(a)
		bl, bc := pos(b)
		if al != bl {
			return al - bl
		}
		return ac - bc
	})
}

// fileConfig mirrors the file. Pointers tell "not set" apart from "set to zero".
type fileConfig struct {
	SettleSeconds *int64        `toml:"settle_seconds"`
	ContextLines  *int64        `toml:"context_lines"`
	Presets       *[]string     `toml:"presets"`
	Pattern       []filePattern `toml:"pattern"`
	Ignore        []fileIgnore  `toml:"ignore"`
	Watch         []fileWatch   `toml:"watch"`
}

type filePattern struct {
	Name  string `toml:"name"`
	Regex string `toml:"regex"`
}

type fileIgnore struct {
	Regex string `toml:"regex"`
}

type fileWatch struct {
	PaneTitle string `toml:"pane_title"`
	Agent     string `toml:"agent"`
}

func unknownKeyErrors(name string, strict *toml.StrictMissingError) []error {
	errs := make([]error, 0, len(strict.Errors))
	for _, de := range strict.Errors {
		line, col := de.Position()
		errs = append(errs, &Error{File: name, Line: line, Column: col,
			Msg: fmt.Sprintf("unknown key %q", strings.Join(de.Key(), "."))})
	}
	return errs
}

var wrongType = regexp.MustCompile(`^cannot decode TOML ([a-z ]+?) into`)

// decodeError turns one of the TOML library's fatal errors into ours, keeping the
// position.
func decodeError(name string, err error) error {
	var de *toml.DecodeError
	if errors.As(err, &de) {
		line, col := de.Position()
		msg := strings.TrimPrefix(de.Error(), "toml: ")
		// The library names our Go struct fields when a value has the wrong type.
		// Say it in terms of the file instead.
		if m := wrongType.FindStringSubmatch(msg); m != nil && len(de.Key()) > 0 {
			msg = fmt.Sprintf("%s has the wrong type (got a TOML %s)", strings.Join(de.Key(), "."), m[1])
		}
		return &Error{File: name, Line: line, Column: col, Msg: msg}
	}
	return &Error{File: name, Msg: strings.TrimPrefix(err.Error(), "toml: ")}
}

// validator checks the decoded values and builds the Config, collecting every
// problem it finds.
type validator struct {
	name      string
	positions map[string]unstable.Position
	errs      []error
}

// fail records a problem at key. If key has no known position it uses the
// nearest parent that does, so no error loses its line.
func (v *validator) fail(key, format string, args ...any) {
	pos, ok := v.positions[key]
	for !ok && key != "" {
		i := strings.LastIndexByte(key, '.')
		if i < 0 {
			break
		}
		key = key[:i]
		pos, ok = v.positions[key]
	}
	v.errs = append(v.errs, &Error{File: v.name, Line: pos.Line, Column: pos.Column,
		Msg: fmt.Sprintf(format, args...)})
}

// number checks an optional whole number is within lo to hi.
func (v *validator) number(key string, value *int64, lo, hi int64, into *int) {
	if value == nil {
		return
	}
	if *value < lo || *value > hi {
		v.fail(key, "%s must be between %d and %d, got %d", key, lo, hi, *value)
		return
	}
	*into = int(*value)
}

func (v *validator) build(raw fileConfig) Config {
	cfg := Default()

	v.number("settle_seconds", raw.SettleSeconds, 1, MaxSettleSeconds, &cfg.SettleSeconds)
	v.number("context_lines", raw.ContextLines, 0, MaxContextLines, &cfg.ContextLines)

	if raw.Presets != nil {
		// An empty list is allowed. It switches every preset off.
		cfg.Presets = []string{}
		for i, p := range *raw.Presets {
			key := "presets." + strconv.Itoa(i)
			switch {
			case !slices.Contains(presetNames, p):
				v.fail(key, "unknown preset %q (known presets: %s)", p, strings.Join(presetNames, ", "))
			case slices.Contains(cfg.Presets, p):
				v.fail(key, "preset %q is listed twice", p)
			default:
				cfg.Presets = append(cfg.Presets, p)
			}
		}
	}

	names := map[string]bool{}
	for i, p := range raw.Pattern {
		key := "pattern." + strconv.Itoa(i)
		name := strings.TrimSpace(p.Name)
		if name == "" {
			v.fail(key, "[[pattern]] needs a name")
		} else if names[name] {
			v.fail(key+".name", "pattern %q is defined twice", name)
		}
		re, ok := v.compile(key, p.Regex, "[[pattern]]")
		if ok && name != "" && !names[name] {
			cfg.Patterns = append(cfg.Patterns, Pattern{Name: name, Regex: re})
		}
		names[name] = true
	}

	for i, ig := range raw.Ignore {
		if re, ok := v.compile("ignore."+strconv.Itoa(i), ig.Regex, "[[ignore]]"); ok {
			cfg.Ignores = append(cfg.Ignores, Ignore{Regex: re})
		}
	}

	titles := map[string]bool{}
	for i, w := range raw.Watch {
		key := "watch." + strconv.Itoa(i)
		title, agent := strings.TrimSpace(w.PaneTitle), strings.TrimSpace(w.Agent)
		if title == "" {
			v.fail(key, "[[watch]] needs a pane_title")
		} else if titles[title] {
			// Two pins for one pane can't both win, so make the choice explicit.
			v.fail(key+".pane_title", "pane %q is pinned twice", title)
		}
		if agent == "" {
			v.fail(key, "[[watch]] needs an agent")
		}
		if title != "" && agent != "" && !titles[title] {
			cfg.Pins = append(cfg.Pins, Pin{PaneTitle: title, Agent: agent})
		}
		titles[title] = true
	}

	return cfg
}

// compile checks a regex from the table at key. An empty regex would match every
// line, which is never what anyone means, so it's an error.
func (v *validator) compile(key, expr, table string) (*regexp.Regexp, bool) {
	if expr == "" {
		v.fail(key, "%s needs a regex", table)
		return nil, false
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		v.fail(key+".regex", "bad regex (patterns use Go's RE2 syntax): %v", err)
		return nil, false
	}
	return re, true
}

// shapeError is a problem with how the file is laid out rather than its values.
type shapeError struct {
	key string
	msg string
}

// caseError is a known key spelt with the wrong case. top is the top level key it
// sits under, as written, so a decode failure on it can be traced back.
type caseError struct {
	top string
	pos unstable.Position
	msg string
}

// schema lists the keys each table allows, with "" for the top level. It's only
// used to catch the wrong case. The decoder's strict mode deals with keys that
// aren't known at all.
var schema = map[string][]string{
	"":        {"settle_seconds", "context_lines", "presets", "pattern", "ignore", "watch"},
	"pattern": {"name", "regex"},
	"ignore":  {"regex"},
	"watch":   {"pane_title", "agent"},
}

// checkCase follows the parts of a key down from table and records every part
// that matches a known key only when case is ignored. It carries on under the
// right spelling, so a wrong case field inside a wrong case table is caught too.
// It returns the right spelling of the first part, or "" if it isn't known.
func (w *documentWalk) checkCase(table string, parts []*unstable.Node) string {
	top, canonFirst := table, ""
	for i, n := range parts {
		got := string(n.Data)
		if i == 0 && table == "" {
			top = got
		}
		next := ""
		for _, want := range schema[table] {
			if strings.EqualFold(want, got) {
				next = want
				break
			}
		}
		if next == "" {
			break
		}
		if next != got {
			w.caseErrors = append(w.caseErrors, caseError{top: top, pos: w.at(n.Raw),
				msg: fmt.Sprintf("unknown key %q (keys are case sensitive, did you mean %s?)", got, next)})
		}
		if i == 0 {
			canonFirst = next
		}
		table = next
	}
	return canonFirst
}

// documentWalk is what walkDocument finds.
type documentWalk struct {
	// positions maps each key to where it is, so validation errors can point at
	// the line. Keys look like "settle_seconds", "presets.2", "pattern.0" for an
	// array table header and "pattern.0.regex" for a value inside it. Values are
	// preferred over keys because that's usually where the mistake is.
	positions map[string]unstable.Position
	// shapeErrors are tables written the wrong way, such as [pattern] for
	// [[pattern]]. The decoder quietly accepts those as a single entry.
	shapeErrors []shapeError
	// caseErrors are keys spelt with the wrong case, such as SETTLE_SECONDS.
	caseErrors []caseError
	// lineStarts holds the byte offset of the start of each line. The parser's own
	// Shape scans from the top of the file for every node, which turns a long file
	// into a stalled reload, so positions are worked out from this instead.
	lineStarts []int
}

func walkDocument(data []byte) documentWalk {
	w := documentWalk{positions: map[string]unstable.Position{}, lineStarts: []int{0}}
	for i, b := range data {
		if b == '\n' {
			w.lineStarts = append(w.lineStarts, i+1)
		}
	}
	counts := map[string]int{}
	// prefix is the current table as written. table is its right spelling, used
	// to check the keys inside it.
	prefix, table := "", ""

	var p unstable.Parser
	p.Reset(data)
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			name, parts := joinKey(e)
			first := firstPart(parts)
			table = w.checkCase("", parts)
			prefix = name
			if e.Kind == unstable.ArrayTable {
				prefix = name + "." + strconv.Itoa(counts[name])
				counts[name]++
			}
			if first != nil {
				w.positions[prefix] = w.at(first.Raw)
				if e.Kind == unstable.Table && slices.Contains(arrayTables, table) {
					w.shapeErrors = append(w.shapeErrors, shapeError{prefix,
						fmt.Sprintf("use [[%s]] (an array of tables), not [%s]", table, name)})
				}
			}
		case unstable.KeyValue:
			name, parts := joinKey(e)
			first := firstPart(parts)
			top := table
			if prefix == "" {
				top = w.checkCase("", parts)
			} else {
				w.checkCase(table, parts)
			}
			key := name
			if prefix != "" {
				key = prefix + "." + name
			}
			if first != nil {
				w.positions[key] = w.at(first.Raw)
			}
			value := e.Value()
			if prefix == "" && slices.Contains(arrayTables, top) &&
				(len(parts) > 1 || value.Kind != unstable.Array) {
				w.shapeErrors = append(w.shapeErrors, shapeError{key,
					fmt.Sprintf("%s must be a list of tables, use [[%s]]", top, top)})
			}
			w.value(top, key, value)
		}
	}
	return w
}

// at turns a node's byte range into a line and column, both starting at 1.
// Columns count bytes, the same as the decoder's own errors.
func (w *documentWalk) at(r unstable.Range) unstable.Position {
	offset := int(r.Offset)
	// The last line that starts at or before offset.
	line := sort.Search(len(w.lineStarts), func(i int) bool { return w.lineStarts[i] > offset }) - 1
	return unstable.Position{Offset: offset, Line: line + 1, Column: offset - w.lineStarts[line] + 1}
}

// causedFailure reports whether a layout error is behind a fatal decode error,
// which is the case when the decoder failed on the same top level key.
func (w *documentWalk) causedFailure(err error) bool {
	var de *toml.DecodeError
	if !errors.As(err, &de) || len(de.Key()) == 0 {
		return false
	}
	for _, s := range w.shapeErrors {
		top, _, _ := strings.Cut(s.key, ".")
		if strings.EqualFold(top, de.Key()[0]) {
			return true
		}
	}
	for _, c := range w.caseErrors {
		if strings.EqualFold(c.top, de.Key()[0]) {
			return true
		}
	}
	return false
}

// value records where a value is, and the values inside it for arrays and inline
// tables. table is the top level key the value sits under, which says which keys
// an inline table inside it may have.
func (w *documentWalk) value(table, key string, n *unstable.Node) {
	if n.Raw.Length > 0 {
		w.positions[key] = w.at(n.Raw)
	}
	switch n.Kind {
	case unstable.Array:
		i := 0
		for it := n.Children(); it.Next(); {
			child := it.Node()
			if child.Kind == unstable.Comment {
				continue
			}
			w.value(table, key+"."+strconv.Itoa(i), child)
			i++
		}
	case unstable.InlineTable:
		for it := n.Children(); it.Next(); {
			child := it.Node()
			if child.Kind != unstable.KeyValue {
				continue
			}
			name, parts := joinKey(child)
			first := firstPart(parts)
			w.checkCase(table, parts)
			childKey := key + "." + name
			if first != nil {
				w.positions[childKey] = w.at(first.Raw)
			}
			w.value(table, childKey, child.Value())
		}
	}
}

// joinKey returns a node's dotted key and the nodes for each part.
func joinKey(e *unstable.Node) (string, []*unstable.Node) {
	var names []string
	var parts []*unstable.Node
	for it := e.Key(); it.Next(); {
		n := it.Node()
		parts = append(parts, n)
		names = append(names, string(n.Data))
	}
	return strings.Join(names, "."), parts
}

func firstPart(parts []*unstable.Node) *unstable.Node {
	if len(parts) == 0 {
		return nil
	}
	return parts[0]
}
