package engine

import "strings"

// Stand-in until #7. The fingerprint is the exact text of the error, so a changed
// timestamp, port or address makes a new entry. #7 replaces it with one that
// ignores the parts that vary.

func fingerprint(lines []string) string {
	return strings.Join(lines, "\n")
}

// group files a finished capture: as a repeat of an entry already in the feed, or
// as a new one.
//
// A repeat of an open entry (new, sent or still failing) always adds to it, so a
// crash loop is one entry with a rising count. A repeat of a dismissed entry
// inside the cooldown is counted but stays dismissed, so dismissing a crash loop
// doesn't bring it straight back. Anything else is a new entry.
func (e *Engine) group(src Source, c *capture) {
	fp := fingerprint(c.lines)
	if r := e.repeatOf(src.ID, fp, c); r != nil {
		r.entry.Count++
		r.entry.LastSeen = c.matchedAt
		r.entry.Lines = c.lines
		r.entry.Before = c.before
		r.entry.After = c.after
		r.entry.MaybeGap = c.maybeGap
		e.recurred(r, c.matchedAt)
		return
	}
	e.add(Entry{
		Source:      src,
		Fingerprint: fp,
		Lines:       c.lines,
		Before:      c.before,
		After:       c.after,
		Count:       1,
		FirstSeen:   c.matchedAt,
		LastSeen:    c.matchedAt,
		State:       StateNew,
		MaybeGap:    c.maybeGap,
	})
}

// repeatOf finds the newest entry c should be counted against, or nil.
func (e *Engine) repeatOf(id SourceID, fp string, c *capture) *record {
	for i := len(e.records) - 1; i >= 0; i-- {
		r := e.records[i]
		if r.entry.Source.ID != id || r.entry.Fingerprint != fp {
			continue
		}
		if r.entry.State.open() {
			return r
		}
		if r.entry.State == StateDismissed && c.matchedAt.Sub(r.entry.LastSeen) <= e.opts.Cooldown {
			return r
		}
		return nil
	}
	return nil
}
