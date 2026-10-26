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
//
// Only the head of a line is matched (see scanned), so one huge line can't stall
// the feed.
func (e *Engine) startsError(line string) (name string, ok bool) {
	line = scanned(line)
	if strings.TrimSpace(line) == "" {
		return "", false
	}
	if e.ignored(line) {
		return "", false
	}
	for _, m := range e.opts.Matchers {
		if m.Match(line) {
			return m.Name(), true
		}
	}
	return "", false
}

// ignored reports whether an ignore rule matches the head of line.
func (e *Engine) ignored(line string) bool {
	line = scanned(line)
	for _, m := range e.opts.Ignores {
		if m.Match(line) {
			return true
		}
	}
	return false
}

// scanned is the head of line that matching looks at: its first maxScannedLine
// bytes, cut without splitting a character. RE2 takes time in proportion to
// what it reads and every preset reads every line, so a 16 MiB line matched
// whole froze the feed for about half a minute (#65). The window keeps the same
// head (see capStored), so what was matched is what reaches the message.
//
// It only ever cuts a line the scrubber has already cleaned, never raw output,
// so it can't split an escape sequence. The scrubber still reads every byte.
//
// A cut head ends in moreText, so a pattern anchored at the end of the line
// with $ can't match where the cut fell. "command not found: \S+$" mustn't
// match a long line just because the cut left one word at the end.
//
// The cost is that an error starting past the first 4 KiB of a line isn't
// caught. The README says so.
func scanned(line string) string {
	head := cut(line, maxScannedLine)
	if len(head) == len(line) {
		return line
	}
	return head + moreText
}

// moreText stands in for the rest of a line cut short for matching: a space and
// a NUL, which the scrubber never lets through. To a pattern it reads as more
// words to come, so \S+$ and \s*$ both fail on it, while .* still runs on to
// the end.
const moreText = " \x00"
