package herdr

import (
	"context"
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
	// the screen was cleared or more was printed than the read covers. Lines
	// then holds every complete line in the new snapshot and some output may
	// have been missed.
	Gap bool
	// Initial marks the first read of a pane. Lines holds whatever was already
	// there, which the caller may want to treat as history rather than news.
	Initial bool
}

// DiffSnapshots returns the complete lines in curr that weren't complete in
// prev. It's pure, so it can be tested without herdr.
//
// When curr holds the pane's whole history, prev's complete lines have to be
// an exact prefix of it, or something was cleared. When curr is truncated the
// window has slid, so the diff looks for the longest run at the start of curr
// that matches the end of prev. Sliding pins the run to the start of curr, so
// repeated lines are only ambiguous when the whole overlap repeats (a pane
// printing the same line hundreds of times). Then new copies of that line can't
// be told from old ones and are undercounted, which is fine for finding errors.
func DiffSnapshots(prev, curr Snapshot) Delta {
	old := prev.complete()
	now := curr.complete()

	if !curr.Truncated {
		if !prev.Truncated && hasPrefix(now, old) {
			return Delta{Lines: now[len(old):]}
		}
		return Delta{Lines: now, Gap: true}
	}

	m := overlap(old, now)
	if !anchored(old, now[:m]) {
		return Delta{Lines: now, Gap: true}
	}
	return Delta{Lines: now[m:]}
}

// overlap returns the largest m where the first m lines of now are the last m
// lines of old.
func overlap(old, now []string) int {
	for m := min(len(old), len(now)); m > 0; m-- {
		if slices.Equal(now[:m], old[len(old)-m:]) {
			return m
		}
	}
	return 0
}

// anchored reports whether a matched run is good enough to trust. All of old
// matching is always enough. Otherwise a run of one line, or of nothing but
// blank lines, could easily be a coincidence after a clear.
func anchored(old, run []string) bool {
	if len(run) == len(old) {
		return true
	}
	if len(run) < 2 {
		return false
	}
	return slices.ContainsFunc(run, func(s string) bool { return strings.TrimSpace(s) != "" })
}

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
//
// A PaneWatcher isn't safe for concurrent use. Give each pane's watcher one
// owner.
type PaneWatcher struct {
	reader PaneReader
	paneID string

	// Lines is how many lines each read asks for.
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
func (w *PaneWatcher) Poll(ctx context.Context) (Delta, error) {
	read, err := w.reader.ReadPane(ctx, w.paneID, w.Lines)
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
	if !w.flushed && w.SettleReads > 0 && w.seen >= w.SettleReads && strings.TrimSpace(last) != "" {
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
