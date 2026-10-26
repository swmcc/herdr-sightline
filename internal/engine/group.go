package engine

import "strings"

// Stand-in until #7. The fingerprint is the exact text of the error, so a changed
// timestamp, port or address makes a new entry. #7 replaces it with one that
// ignores the parts that vary.

// groups is #7's bookkeeping. For now it's an index of the newest entry for each
// source and fingerprint, so a repeat is found without scanning every entry. #7
// can add cooldown timers here.
type groups struct {
	newest map[groupKey]*record
}

type groupKey struct {
	source      SourceID
	fingerprint string
}

func fingerprint(lines []string) string {
	return strings.Join(lines, "\n")
}

// group files a caught error: as a repeat of an entry already in the feed, or as
// a new one.
//
// A repeat of an open entry (new, sent or still failing) always adds to it, so a
// crash loop is one entry with a rising count. A repeat of a dismissed entry
// inside the cooldown is counted but stays dismissed, so dismissing a crash loop
// doesn't bring it straight back. Anything else is a new entry.
func (e *Engine) group(src Source, f found) {
	key := groupKey{src.ID, fingerprint(f.lines)}
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
	e.groups.newest[key] = r
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
