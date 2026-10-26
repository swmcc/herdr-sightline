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
	// that cares (the fix check) should treat it as "can't tell" and look for
	// other signs, such as a scroll event (#45).
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
// never proves no output. #45 is about using scroll events to close the gap.
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
// A PaneWatcher isn't safe for concurrent use. Give each pane's watcher one
// owner.
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
	read, err := w.reader.ReadPane(ctx, w.paneID, max(w.Lines, MinWatchLines))
	if errors.Is(err, ErrTooLarge) {
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
	completed := len(d.Lines)

	// A last line reported early is now usually the first completed line. Drop
	// it if it finished as it was, so it isn't reported twice.
	if w.flushed && !d.Gap && completed > 0 && d.Lines[0] == w.pending {
		d.Lines = d.Lines[1:]
	}

	last := curr.last()
	if w.started && !d.Gap && completed == 0 && last == w.pending {
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
