package engine

import "strings"

// Matcher decides whether a line starts an error. Name is shown in the feed.
type Matcher interface {
	Name() string
	Match(line string) bool
}

// Substring matches any line containing text.
//
// Stand-in until #6 brings RE2 patterns and the built-in presets.
type Substring struct {
	Label string
	Text  string
}

// Name returns the label shown in the feed.
func (m Substring) Name() string { return m.Label }

// Match reports whether line contains the text. Empty text never matches, so a
// blank setting can't turn every line into an error.
func (m Substring) Match(line string) bool {
	return m.Text != "" && strings.Contains(line, m.Text)
}

// startsError reports whether line should start a capture, and the name of the
// matcher that caught it. Ignores win over matchers.
func (e *Engine) startsError(line string) (name string, ok bool) {
	if strings.TrimSpace(line) == "" {
		return "", false
	}
	for _, m := range e.opts.Ignores {
		if m.Match(line) {
			return "", false
		}
	}
	for _, m := range e.opts.Matchers {
		if m.Match(line) {
			return m.Name(), true
		}
	}
	return "", false
}
