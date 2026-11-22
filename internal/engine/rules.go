package engine

import "time"

// Rules are error patterns and ignore rules for one source, on top of the ones
// in Options. The adapter uses them for a repo's own project file (#22), which
// only applies to the panes in that repo.
//
// They're weaker than Options on purpose, because someone other than the person
// may have written them. Options' ignore rules are checked first, then every
// matcher, and only then a source's own ignore rules. A line one of those hides
// is counted (see Hidden), so the feed can show what they kept from view.
type Rules struct {
	Matchers []Matcher
	Ignores  []Matcher
}

// Hidden is a line a source's own ignore rule kept from starting an error.
type Hidden struct {
	Source SourceID
	Line   string // the line as kept, cut like any other (see capStored)
	At     time.Time
}

// maxHidden is how many hidden lines are kept to show. The count goes on
// past it.
const maxHidden = 50

// SetRules gives a watched source its own rules, replacing any it had. A
// source that isn't watched is ignored, and Unwatch drops them. They follow
// the source through Rename.
//
// They apply from the next line fed. Unlike a new ignore rule in Options, a
// source's ignore rule never dismisses an entry already filed, and never
// touches a capture in progress.
func (e *Engine) SetRules(id SourceID, r Rules) {
	if s, ok := e.sources[id]; ok {
		s.rules = Rules{Matchers: clip(r.Matchers), Ignores: clip(r.Ignores)}
	}
}

func clip(ms []Matcher) []Matcher { return append([]Matcher(nil), ms...) }

// Hidden is how many lines sources' own ignore rules have hidden since the
// engine started, and the latest of them, oldest first.
func (e *Engine) Hidden() (total int, recent []Hidden) {
	return e.hiddenTotal, append([]Hidden(nil), e.hidden...)
}

// hide records a line a source's own ignore rule hid. A fix being checked on
// the source can't be called fixed after it, because the line hidden could be
// the error coming back.
func (e *Engine) hide(s *source, line string, now time.Time) {
	e.hiddenTotal++
	e.hidden = append(e.hidden, Hidden{Source: s.info.ID, Line: line, At: now})
	if extra := len(e.hidden) - maxHidden; extra > 0 {
		e.hidden = append(e.hidden[:0:0], e.hidden[extra:]...)
	}
	e.uncertainRead(s.info.ID, now, false)
}
