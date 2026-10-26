package engine

import (
	"regexp"
	"strings"
)

// Matcher decides whether a line starts an error. Name is shown in the feed.
type Matcher interface {
	Name() string
	Match(line string) bool
}

// Regexp returns a Matcher for an RE2 pattern, such as a [[pattern]] or [[ignore]]
// entry from sightline.toml. A nil re never matches, so a missing pattern can't
// turn every line into an error.
func Regexp(name string, re *regexp.Regexp) Matcher {
	return regexpMatcher{name: name, re: re}
}

type regexpMatcher struct {
	name string
	re   *regexp.Regexp
}

func (m regexpMatcher) Name() string { return m.name }

func (m regexpMatcher) Match(line string) bool {
	return m.re != nil && m.re.MatchString(line)
}

// Substring matches any line containing text. It's handy in tests, where a word
// is clearer than a pattern. Real matchers come from [Presets] and [Regexp].
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
