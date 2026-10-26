package herdr

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// herdr only offers snapshots of a pane, not a stream of its output (#1). So to
// find new lines, Sightline reads the last few hundred lines over and over and
// works out what was added since the last read. This file is that diff.

// DefaultWatchLines is how many lines a PaneWatcher reads each time. It has to
// be more than a pane can print between two reads, or the diff loses its place
// and reports a gap.
const DefaultWatchLines = 300

// MinWatchLines is the fewest lines a PaneWatcher will ask for.
const MinWatchLines = 3

// DefaultSettleReads is how many reads in a row the last line has to stay the
// same before a PaneWatcher treats it as finished. See PaneWatcher.
const DefaultSettleReads = 2

// Snapshot is one read of a pane, split into lines.
type Snapshot struct {
	Lines []string
	// Truncated is true when the pane had more history than the read covered,
	// so the first line isn't the start of the pane's output.
	Truncated bool
}

// SnapshotOf splits a PaneRead into lines.
func SnapshotOf(r PaneRead) Snapshot {
	return Snapshot{Lines: strings.Split(r.Text, "\n"), Truncated: r.Truncated}
}

// complete returns every line but the last. herdr's text never ends in a
// newline, so the last line might still be being written. The lines before it
// have a newline after them and won't change.
func (s Snapshot) complete() []string {
	if len(s.Lines) == 0 {
		return nil
	}
	return s.Lines[:len(s.Lines)-1]
}

// last returns the line that might still be being written.
func (s Snapshot) last() string {
	if len(s.Lines) == 0 {
		return ""
	}
	return s.Lines[len(s.Lines)-1]
}

// Delta is what a pane printed between two snapshots.
type Delta struct {
	// Lines are the complete lines that are new since the previous snapshot,
	// oldest first.
	Lines []string
	// Gap means the previous snapshot couldn't be found in this one, because
	// the screen was cleared, more was printed than the read covers or a line
	// above the last one was rewritten (a progress bar or status line). Some
	// output may have been missed.
	//
	// Lines then holds the new snapshot's complete lines, less any it starts
	// with that the previous snapshot also started with when both start at the
	// same place in the pane's history. They can still include
	// lines that were reported before, so on a gap they must not be counted as
	// repeats or as an error coming back.
	//
	// A PaneWatcher also sets Gap when a read finds nothing new but scroll
	// hints say at least a window's worth of rows moved into the pane's
	// history (see PaneWatcher.Hint).
	Gap bool
	// Initial marks the first read of a pane. Lines holds whatever was already
	// there, last line included unless it's blank, which the caller may want to
	// treat as history rather than news.
	Initial bool
	// Ambiguous means the end of the previous snapshot matches more than one
	// place in this one, or only matched somewhere too short to trust. That
	// happens when a pane keeps printing the same lines, such as a crash loop
	// with no timestamps. Lines then holds the fewest new lines that fit,
	// possibly none, so a repeat of the same output can go unseen. A caller
	// that cares (the fix check) should treat it as "can't tell".
	//
	// A PaneWatcher also sets Ambiguous when a read finds nothing new but
	// scroll hints say the pane's history changed since the last read (see
	// PaneWatcher.Hint).
	Ambiguous bool
}

// DiffSnapshots returns the complete lines in curr that weren't complete in
// prev. It's pure, so it can be tested without herdr.
//
// When curr holds the pane's whole history, prev's complete lines have to be
// an exact prefix of it, or something was cleared. When curr is truncated the
// window has slid, so the diff looks for the longest run at the start of curr
// that matches the end of prev. Sliding pins the run to the start of curr, so
// repeated lines only confuse it when the window repeats, and then Ambiguous
// is set.
//
// Snapshots alone have a blind spot. If the new snapshot is identical to the
// old one, nothing was printed or a whole window of identical output was: the
// screen cleared and the same text printed again, or a crash loop whose block
// is at least as long as the window. The two can't be told apart, and
// flagging every unchanged read would fire on every idle poll. So no lines
// never proves no output. PaneWatcher narrows the blind spot with scroll hints,
// but can't close it (see PaneWatcher.Hint).
func DiffSnapshots(prev, curr Snapshot) Delta {
	old := prev.complete()
	now := curr.complete()

	if !curr.Truncated {
		if !prev.Truncated && hasPrefix(now, old) {
			return Delta{Lines: now[len(old):]}
		}
		return gap(prev, curr, old, now, false)
	}

	// If prev held the whole history, its first line has since scrolled out of
	// a truncated read, so all of old can't legitimately match.
	limit := len(old)
	if !prev.Truncated {
		limit--
	}
	best, others := overlaps(old, now, limit)
	if best == 0 {
		return gap(prev, curr, old, now, others)
	}
	return Delta{Lines: now[best:], Ambiguous: others}
}

// gap builds a Gap delta. When both snapshots start at the same place (both
// whole history, or both truncated), lines they both start with were seen last
// time, so they're left out to avoid sending the whole window again every time
// a status line changes. When one is truncated and the other isn't, they can't
// start at the same place and any shared start is a coincidence.
func gap(prev, curr Snapshot, old, now []string, ambiguous bool) Delta {
	n := 0
	if prev.Truncated == curr.Truncated {
		for n < len(old) && n < len(now) && old[n] == now[n] {
			n++
		}
	}
	return Delta{Lines: now[n:], Gap: true, Ambiguous: ambiguous}
}

// overlaps looks for runs where the first m lines of now are the last m lines
// of old, with m up to limit. best is the largest run good enough to trust, or
// 0 if there's none. others reports whether any other run matched too, trusted
// or not, which makes the match ambiguous.
//
// All of old matching (only allowed when prev was truncated) is always trusted.
// Otherwise a run of one line, or of nothing but blank lines, could easily be a
// coincidence after a clear, so it isn't trusted. A run of nothing but blank
// lines doesn't count as another match either, since blank lines are
// everywhere and would make almost every diff ambiguous.
func overlaps(old, now []string, limit int) (best int, others bool) {
	for m := min(limit, len(now)); m > 0; m-- {
		blank := !slices.ContainsFunc(now[:m], notBlank)
		if best != 0 && blank {
			continue
		}
		if !slices.Equal(now[:m], old[len(old)-m:]) {
			continue
		}
		trusted := m == len(old) || (m >= 2 && !blank)
		switch {
		case best == 0 && trusted:
			best = m
		case !blank:
			return best, true
		}
	}
	return best, false
}

func notBlank(s string) bool { return strings.TrimSpace(s) != "" }

func hasPrefix(lines, prefix []string) bool {
	return len(lines) >= len(prefix) && slices.Equal(lines[:len(prefix)], prefix)
}

// PaneReader reads a pane. Client is one.
type PaneReader interface {
	ReadPane(ctx context.Context, paneID string, lines int) (PaneRead, error)
}

// PaneWatcher reads one pane on demand and returns what's new each time.
//
// The last line of a read has no newline after it yet, so it may be half
// written. A PaneWatcher holds it back until a line follows it, or until it has
// stayed the same for SettleReads reads in a row. Without the second rule an
// error that's the last thing a quiet server prints would never be reported,
// because herdr drops the empty cursor line after it. The cost is that a line
// which stalls part way for that long is reported twice: once as it was, and
// again in full when it's finished. Blank last lines are never reported early.
// The first read is the exception: its last line is included in the Initial
// delta, since that's all history anyway.
//
// Snapshots alone can't tell an idle pane from one that printed a whole window
// of the same output (see DiffSnapshots). So the owner should also subscribe
// to ScrollChanged for the pane and pass each event's scroll to Hint. A read
// that finds nothing new while the hints say the history moved then comes back
// Ambiguous or Gap instead of looking quiet. The watcher has no goroutine of
// its own and never subscribes itself, so it stays easy to test.
//
// A PaneWatcher isn't safe for concurrent use. Give each pane's watcher one
// owner, and call Hint and Poll from that owner.
type PaneWatcher struct {
	reader PaneReader
	paneID string

	// Lines is how many lines each read asks for. Anything below
	// MinWatchLines is raised to it, because the diff needs at least two
	// complete lines to find its place, plus the last line.
	Lines int
	// SettleReads is how many reads in a row the last line has to stay the same
	// before it's reported without a line after it. Zero means never.
	SettleReads int

	prev    Snapshot
	started bool
	pending string
	seen    int
	flushed bool

	// The last scroll hint, and what the hints since the last read add up to:
	// rows that moved into history with the viewport unchanged (grew), a fall
	// to 0 (cleared) and any other change that isn't growth (changed). credit
	// is how many rows the last read already covered.
	scroll      ScrollInfo
	scrollKnown bool
	grew        uint64
	cleared     bool
	changed     bool
	credit      uint64
}

// Hint tells the watcher where herdr says the pane is scrolled to, as a
// pane.scroll_changed event reports it. It returns true when the pane's history
// changed, which usually means it printed something, so the owner may want to
// Poll sooner than usual. Hints only change what the next Poll reports when
// that read finds nothing new:
//
//   - If max_offset_from_bottom rose by at least Lines rows since the last
//     read, it's a Gap. A window's worth of output went by unseen.
//   - If it rose by less, or fell to 0, it's Ambiguous. Something was printed
//     or cleared, and the read can't say what.
//   - Any other change (a fall above 0, the viewport height changing or the
//     first hint) is Ambiguous only if the read before left no credit, for
//     the reason below.
//
// herdr sends the event in batches about 100ms apart, so the hint for a burst
// often lands after the Poll that already reported the burst. So that it
// doesn't make the quiet read after it look busy, the read leaves credit for
// the lines it reported that its own hints hadn't already explained (a whole
// window after a gap), and the next read takes that off the growth first. A
// late fall above 0 is let off while there's credit, because at the cap output
// makes the offset fall (see below). The credit lasts one read only, since a
// credit that waited for a hint could later swallow real output. So the owner
// shouldn't poll much faster than herdr's batching, or the credit is spent on
// a read before the late hint arrives.
//
// Some late hints still make the quiet read after them Ambiguous or a Gap. The
// offset counts screen rows, not lines, so a hint for long lines that wrapped
// can come to more than the lines it describes. A burst bigger than the window
// gives a late hint bigger than the credit. And a clear is never let off, so a
// watcher that clears the screen on every rerun gives one Ambiguous read after
// each. All of these err on the side of "can't tell". Spotting a clear is best
// effort anyway: a clear and reprint inside one batch can arrive as an
// ordinary rise or fall, never showing 0.
//
// What herdr 0.9.1 does with the offset, checked live for #45:
//
//   - A clear of the history (ESC[3J) resets it to 0. Clearing the screen alone
//     (ESC[2J) leaves it be.
//   - At the history cap, around 10,800 to 11,100 rows of short lines, it saw
//     tooths. It rises as lines arrive, then drops by a few hundred rows at
//     once when old history is freed. So a fall can hide output, and a fall is
//     never counted as growth.
//   - Shrinking the pane's height pushes rows into history without output, so
//     a rise only counts when the viewport height stayed the same.
//   - Narrowing the pane re-wraps long lines in history and raises the offset
//     with the viewport height unchanged (125 rows became 288). ScrollInfo has
//     no width, so that rise counts as growth and can make a quiet read a Gap.
//     That's a false "can't tell", never a false "nothing happened".
//
// The first hint has nothing to compare with. herdr only sends the event when
// something changed, so it counts as a change, but not as growth.
//
// Hints can't close the blind spot. herdr doesn't send the event until the
// screen is full and lines start moving into history, so output that fits on
// one screen (a clear and reprint of a short failure, say) still looks quiet.
// So no new lines still never proves no output.
func (w *PaneWatcher) Hint(s ScrollInfo) bool {
	prev, known := w.scroll, w.scrollKnown
	w.scroll, w.scrollKnown = s, true
	switch {
	case !known:
		w.changed = true
	case s.MaxOffsetFromBottom == prev.MaxOffsetFromBottom:
		return false
	case s.ViewportRows != prev.ViewportRows:
		w.changed = true
	case s.MaxOffsetFromBottom > prev.MaxOffsetFromBottom:
		w.grew += s.MaxOffsetFromBottom - prev.MaxOffsetFromBottom
	case s.MaxOffsetFromBottom == 0:
		w.cleared = true
	default:
		w.changed = true
	}
	return true
}

// window is how many lines each read asks for.
func (w *PaneWatcher) window() int { return max(w.Lines, MinWatchLines) }

// forgetHints clears what the hints since the last read add up to, and gives
// the next read credit for rows this one already covered.
func (w *PaneWatcher) forgetHints(credit uint64) {
	w.grew, w.cleared, w.changed, w.credit = 0, false, false, credit
}

// applyHints marks d Ambiguous or Gap if it found nothing new but the hints
// since the last read say the history moved (see Hint), then works out the
// credit for the next read. The first read is all history anyway, so hints
// before it don't count.
func (w *PaneWatcher) applyHints(d *Delta) {
	window := uint64(w.window())
	if d.Gap {
		w.forgetHints(window)
		return
	}
	if d.Initial {
		w.forgetHints(uint64(len(d.Lines)))
		return
	}
	unexplained := w.grew - min(w.grew, w.credit)
	if reported := uint64(len(d.Lines)); reported > 0 {
		w.forgetHints(reported - min(reported, unexplained))
		return
	}
	switch {
	case unexplained >= window:
		d.Gap, d.Ambiguous = true, false
	case unexplained > 0 || w.cleared || (w.changed && w.credit == 0):
		d.Ambiguous = true
	}
	w.forgetHints(0)
}

// NewPaneWatcher returns a watcher for paneID using DefaultWatchLines and
// DefaultSettleReads.
func NewPaneWatcher(reader PaneReader, paneID string) *PaneWatcher {
	return &PaneWatcher{
		reader:      reader,
		paneID:      paneID,
		Lines:       DefaultWatchLines,
		SettleReads: DefaultSettleReads,
	}
}

// PaneID is the pane this watcher reads.
func (w *PaneWatcher) PaneID() string { return w.paneID }

// Poll reads the pane once and returns what it printed since the last Poll.
// If the pane has closed, the error satisfies IsPaneNotFound.
//
// If the read is bigger than the client will accept (ErrTooLarge), Poll
// reports a Gap with no lines rather than an error, and keeps diffing against
// the last read it could use. Otherwise a pane full of very long lines would
// fail every poll for ever. Lowering Lines makes the reads smaller.
func (w *PaneWatcher) Poll(ctx context.Context) (Delta, error) {
	read, err := w.reader.ReadPane(ctx, w.paneID, w.window())
	if errors.Is(err, ErrTooLarge) {
		// The gap covers whatever the hints so far describe.
		w.forgetHints(uint64(w.window()))
		return Delta{Gap: true}, nil
	}
	if err != nil {
		return Delta{}, fmt.Errorf("watching pane %s: %w", w.paneID, err)
	}
	return w.next(SnapshotOf(read)), nil
}

// next folds one snapshot into the watcher's state. It's split from Poll so the
// held back last line can be tested without a reader.
func (w *PaneWatcher) next(curr Snapshot) Delta {
	var d Delta
	if w.started {
		d = DiffSnapshots(w.prev, curr)
	} else {
		d = Delta{Lines: curr.complete(), Initial: true}
	}
	diffGap := d.Gap
	completed := len(d.Lines)
	w.applyHints(&d)

	// A last line reported early is now usually the first completed line. Drop
	// it if it finished as it was, so it isn't reported twice.
	if w.flushed && !diffGap && completed > 0 && d.Lines[0] == w.pending {
		d.Lines = d.Lines[1:]
	}

	// A gap only the hints found leaves the held back line alone, so a line
	// that was already reported isn't sent again later outside a gap.
	last := curr.last()
	if w.started && !diffGap && completed == 0 && last == w.pending {
		w.seen++
	} else {
		w.pending, w.seen, w.flushed = last, 1, false
	}
	// The first read's last line goes out with the rest of the first read, so
	// stale output is never later mistaken for news by settling.
	settled := d.Initial || (w.SettleReads > 0 && w.seen >= w.SettleReads)
	if !w.flushed && settled && notBlank(last) {
		d.Lines = append(slices.Clip(d.Lines), last)
		w.flushed = true
	}

	w.prev, w.started = curr, true
	// Copy, so the caller can't change the snapshot kept for the next diff.
	d.Lines = slices.Clone(d.Lines)
	if len(d.Lines) == 0 {
		d.Lines = nil
	}
	return d
}
