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
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// Error is one problem with the file. Line and Column start at 1 and are 0 when
// the position isn't known.
type Error struct {
	File   string
	Line   int
	Column int
	Msg    string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.File)
	if e.Line > 0 {
		b.WriteString(":" + strconv.Itoa(e.Line))
		if e.Column > 0 {
			b.WriteString(":" + strconv.Itoa(e.Column))
		}
	}
	b.WriteString(": " + e.Msg)
	return b.String()
}

// Load reads the config file from dir. A missing file isn't an error, it means the
// defaults. Problems with the file come back as one or more *Error values joined
// together, so every mistake is reported at once rather than one per reload.
func Load(dir string) (Config, error) {
	path := Path(dir)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("reading config: %w", err)
	}
	return Parse(path, data)
}

// Parse turns the contents of a config file into a Config. name is only used in
// error messages.
func Parse(name string, data []byte) (Config, error) {
	var raw fileConfig
	dec := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return Config{}, decodeErrors(name, err)
	}

	positions, err := keyPositions(data)
	if err != nil {
		// The decoder has already accepted the document, so this shouldn't happen.
		// Carry on without positions rather than refuse a good file.
		positions = map[string]unstable.Position{}
	}
	v := validator{name: name, positions: positions}
	cfg := v.build(raw)
	if len(v.errs) > 0 {
		return Config{}, errors.Join(v.errs...)
	}
	return cfg, nil
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

var wrongType = regexp.MustCompile(`^cannot decode TOML (\w+) into`)

// decodeErrors turns the TOML library's errors into ours, keeping the positions.
func decodeErrors(name string, err error) error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		errs := make([]error, 0, len(strict.Errors))
		for _, de := range strict.Errors {
			line, col := de.Position()
			errs = append(errs, &Error{File: name, Line: line, Column: col,
				Msg: fmt.Sprintf("unknown key %q", strings.Join(de.Key(), "."))})
		}
		return errors.Join(errs...)
	}
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

func (v *validator) fail(key, format string, args ...any) {
	pos := v.positions[key]
	v.errs = append(v.errs, &Error{File: v.name, Line: pos.Line, Column: pos.Column,
		Msg: fmt.Sprintf(format, args...)})
}

func (v *validator) build(raw fileConfig) Config {
	cfg := Default()

	if raw.SettleSeconds != nil {
		if *raw.SettleSeconds < 1 {
			v.fail("settle_seconds", "settle_seconds must be at least 1, got %d", *raw.SettleSeconds)
		} else {
			cfg.SettleSeconds = int(*raw.SettleSeconds)
		}
	}
	if raw.ContextLines != nil {
		if *raw.ContextLines < 0 {
			v.fail("context_lines", "context_lines can't be negative, got %d", *raw.ContextLines)
		} else {
			cfg.ContextLines = int(*raw.ContextLines)
		}
	}

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

	seen := map[string]bool{}
	for i, p := range raw.Pattern {
		key := "pattern." + strconv.Itoa(i)
		name := strings.TrimSpace(p.Name)
		if name == "" {
			v.fail(key, "[[pattern]] needs a name")
		} else if seen[name] {
			v.fail(key+".name", "pattern %q is defined twice", name)
		}
		seen[name] = true
		re, ok := v.compile(key, p.Regex, "[[pattern]]")
		if ok && name != "" {
			cfg.Patterns = append(cfg.Patterns, Pattern{Name: name, Regex: re})
		}
	}

	for i, ig := range raw.Ignore {
		if re, ok := v.compile("ignore."+strconv.Itoa(i), ig.Regex, "[[ignore]]"); ok {
			cfg.Ignores = append(cfg.Ignores, Ignore{Regex: re})
		}
	}

	for i, w := range raw.Watch {
		key := "watch." + strconv.Itoa(i)
		title, agent := strings.TrimSpace(w.PaneTitle), strings.TrimSpace(w.Agent)
		if title == "" {
			v.fail(key, "[[watch]] needs a pane_title")
		}
		if agent == "" {
			v.fail(key, "[[watch]] needs an agent")
		}
		if title != "" && agent != "" {
			cfg.Pins = append(cfg.Pins, Pin{PaneTitle: title, Agent: agent})
		}
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

// keyPositions maps each key in the document to where it is, so validation errors
// can point at the line. Keys look like "settle_seconds", "presets.2", "pattern.0"
// for an array table header and "pattern.0.regex" for a value inside it. Values are
// preferred over keys because that's usually where the mistake is.
func keyPositions(data []byte) (map[string]unstable.Position, error) {
	positions := map[string]unstable.Position{}
	counts := map[string]int{}
	prefix := ""

	var p unstable.Parser
	p.Reset(data)
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			name, first := joinKey(e)
			prefix = name
			if e.Kind == unstable.ArrayTable {
				prefix = name + "." + strconv.Itoa(counts[name])
				counts[name]++
			}
			if first != nil {
				positions[prefix] = p.Shape(first.Raw).Start
			}
		case unstable.KeyValue:
			name, first := joinKey(e)
			key := name
			if prefix != "" {
				key = prefix + "." + name
			}
			if first != nil {
				positions[key] = p.Shape(first.Raw).Start
			}
			value := e.Value()
			if value.Raw.Length > 0 {
				positions[key] = p.Shape(value.Raw).Start
			}
			if value.Kind == unstable.Array {
				i := 0
				for it := value.Children(); it.Next(); {
					n := it.Node()
					if n.Kind == unstable.Comment {
						continue
					}
					if n.Raw.Length > 0 {
						positions[key+"."+strconv.Itoa(i)] = p.Shape(n.Raw).Start
					}
					i++
				}
			}
		}
	}
	return positions, p.Error()
}

// joinKey returns a node's dotted key and the node for its first part.
func joinKey(e *unstable.Node) (string, *unstable.Node) {
	var parts []string
	var first *unstable.Node
	for it := e.Key(); it.Next(); {
		n := it.Node()
		if first == nil {
			first = n
		}
		parts = append(parts, string(n.Data))
	}
	return strings.Join(parts, "."), first
}
