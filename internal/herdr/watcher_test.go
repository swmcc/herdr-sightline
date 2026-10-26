package herdr

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// snap builds a snapshot from text the way herdr returns it: lines joined with
// newlines and no newline at the end.
func snap(text string, truncated bool) Snapshot {
	return SnapshotOf(PaneRead{Text: text, Truncated: truncated})
}

// numbered returns "line from" to "line to" joined with newlines.
func numbered(from, to int) string {
	var lines []string
	for i := from; i <= to; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	return strings.Join(lines, "\n")
}

func numberedLines(from, to int) []string {
	return strings.Split(numbered(from, to), "\n")
}

func TestDiffSnapshots(t *testing.T) {
	tests := []struct {
		name          string
		prev          Snapshot
		curr          Snapshot
		want          []string
		wantGap       bool
		wantAmbiguous bool
	}{
		{
			name: "appended lines",
			prev: snap("a\nb\nc", false),
			curr: snap("a\nb\nc\nd\ne", false),
			want: []string{"c", "d"},
		},
		{
			name: "no change",
			prev: snap("a\nb\nc", false),
			curr: snap("a\nb\nc", false),
		},
		{
			name: "partial last line still growing is held back",
			prev: snap("a\nb\nerror: bo", false),
			curr: snap("a\nb\nerror: boom", false),
		},
		{
			name: "partial last line is reported once a line follows it",
			prev: snap("a\nb\nerror: bo", false),
			curr: snap("a\nb\nerror: boom\n$ ", false),
			want: []string{"error: boom"},
		},
		{
			name: "first text in an empty pane",
			prev: snap("", false),
			curr: snap("a\nb", false),
			want: []string{"a"},
		},
		{
			name: "identical repeated lines in a short pane are all counted",
			prev: snap("ok\nok", false),
			curr: snap("ok\nok\nok\nok", false),
			want: []string{"ok", "ok"},
		},
		{
			name: "window slides",
			prev: snap(numbered(1, 20), true),
			curr: snap(numbered(6, 25), true),
			want: numberedLines(20, 24),
		},
		{
			name: "window starts sliding",
			prev: snap(numbered(1, 15), false),
			curr: snap(numbered(6, 25), true),
			want: numberedLines(15, 24),
		},
		{
			name: "repeated lines at the end of a sliding window",
			prev: snap("a\nb\nx\nx\nx", true),
			curr: snap("b\nx\nx\nx\nx", true),
			want: []string{"x"},
		},
		{
			name:          "a window of one repeated line is ambiguous",
			prev:          snap(strings.Repeat(".\n", 9)+".", true),
			curr:          snap(strings.Repeat(".\n", 9)+".", true),
			wantAmbiguous: true,
		},
		{
			name:          "a crash loop repeating a block is ambiguous",
			prev:          snap(strings.Repeat("Error: boom\n    at a\n    at b\n", 4)+"$ ", true),
			curr:          snap(strings.Repeat("Error: boom\n    at a\n    at b\n", 4)+"$ ", true),
			wantAmbiguous: true,
		},
		{
			name:    "an empty pane then more than the window holds",
			prev:    snap("", false),
			curr:    snap(numbered(1, 10), true),
			want:    numberedLines(1, 9),
			wantGap: true,
		},
		{
			name:    "a lone line then more than the window holds",
			prev:    snap("$ \nmake", false),
			curr:    snap("$ \nmore\nstuff", true),
			want:    []string{"$ ", "more"},
			wantGap: true,
		},
		{
			name:    "cleared screen",
			prev:    snap("a\nb\nc\nd", false),
			curr:    snap("$ ", false),
			wantGap: true,
		},
		{
			name:    "cleared screen then new output",
			prev:    snap("a\nb\nc", false),
			curr:    snap("x\ny\n$ ", false),
			want:    []string{"x", "y"},
			wantGap: true,
		},
		{
			name:    "history shrank",
			prev:    snap(numbered(1, 20), true),
			curr:    snap(numbered(1, 5), false),
			want:    numberedLines(1, 4),
			wantGap: true,
		},
		{
			name:    "more output than the window holds",
			prev:    snap(numbered(1, 10), true),
			curr:    snap(numbered(50, 59), true),
			want:    numberedLines(50, 58),
			wantGap: true,
		},
		{
			name:          "a single matching line isn't trusted",
			prev:          snap("build\nok\n$ \nlater", true),
			curr:          snap("$ \nsomething\nelse\nnow", true),
			want:          []string{"$ ", "something", "else"},
			wantGap:       true,
			wantAmbiguous: true,
		},
		{
			// The true overlap is one line (E), which is too short to trust. The
			// two line run E,E matches by chance and drops the first new E, so
			// it's flagged.
			name:          "a chance two line match near overflow is ambiguous",
			prev:          snap("a\nb\nc\nE\nE\nE", true),
			curr:          snap("E\nE\nX\nY\nZ\nW", true),
			want:          []string{"X", "Y", "Z"},
			wantAmbiguous: true,
		},
		{
			name:    "a rewritten status line doesn't resend the lines above it",
			prev:    snap("a\nb\nstatus 1\nc", false),
			curr:    snap("a\nb\nstatus 2\nc\nd", false),
			want:    []string{"status 2", "c"},
			wantGap: true,
		},
		{
			name:    "a rewritten line in a full window doesn't resend the lines above it",
			prev:    snap(numbered(1, 5)+"\n50%\n"+numbered(6, 10), true),
			curr:    snap(numbered(1, 5)+"\n90%\n"+numbered(6, 10), true),
			want:    append([]string{"90%"}, numberedLines(6, 9)...),
			wantGap: true,
		},
		// Blind spots: an unchanged snapshot can hide a whole window of
		// identical output, and nothing in the snapshots can tell it from
		// nothing being printed. These pin today's behaviour (no lines, no gap,
		// not ambiguous) until #45 brings in scroll events to catch them.
		{
			name: "blind spot: clear and the same text printed again",
			prev: snap("a\nb\n$ ", false),
			curr: snap("a\nb\n$ ", false),
		},
		{
			name: "blind spot: a crash loop whose block is the whole window",
			prev: snap("e1\ne2\ne3\ne4\ne5\ne6", true),
			curr: snap("e1\ne2\ne3\ne4\ne5\ne6", true),
		},
		{
			// Unlike the two above, a window that repeats within itself is at
			// least flagged.
			name:          "blind spot: a window that shifts by its own period",
			prev:          snap("H\nE\nH\nE\nH\nE", true),
			curr:          snap("H\nE\nH\nE\nH\nE", true),
			wantAmbiguous: true,
		},
		{
			name:    "a run of blank lines isn't trusted",
			prev:    snap("build\nok\n\n\nlater", true),
			curr:    snap("\n\nfresh\nnow", true),
			want:    []string{"", "", "fresh"},
			wantGap: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DiffSnapshots(tt.prev, tt.curr)
			if !slices.Equal(got.Lines, tt.want) {
				t.Errorf("lines = %q, want %q", got.Lines, tt.want)
			}
			if got.Gap != tt.wantGap {
				t.Errorf("gap = %v, want %v", got.Gap, tt.wantGap)
			}
			if got.Ambiguous != tt.wantAmbiguous {
				t.Errorf("ambiguous = %v, want %v", got.Ambiguous, tt.wantAmbiguous)
			}
		})
	}
}

func TestPaneWatcherNext(t *testing.T) {
	type step struct {
		snap    Snapshot
		want    []string
		gap     bool
		initial bool
	}
	tests := []struct {
		name   string
		settle int
		steps  []step
	}{
		{
			name:   "first read returns what's already there",
			settle: 2,
			steps: []step{
				{snap: snap("a\nb\n$ ", false), want: []string{"a", "b", "$ "}, initial: true},
			},
		},
		{
			name:   "a stale last line from before watching isn't news later",
			settle: 2,
			steps: []step{
				{snap: snap("a\nError: listen EADDRINUSE", false), want: []string{"a", "Error: listen EADDRINUSE"}, initial: true},
				{snap: snap("a\nError: listen EADDRINUSE", false)},
				{snap: snap("a\nError: listen EADDRINUSE", false)},
				{snap: snap("a\nError: listen EADDRINUSE\n$ ", false)},
			},
		},
		{
			name:   "a growing last line waits for the line after it",
			settle: 2,
			steps: []step{
				{snap: snap("a\n$ ", false), want: []string{"a", "$ "}, initial: true},
				{snap: snap("a\n$ \nTypeEr", false)},
				{snap: snap("a\n$ \nTypeError: x", false)},
				{snap: snap("a\n$ \nTypeError: x is undefined\n    at main", false), want: []string{"TypeError: x is undefined"}},
			},
		},
		{
			name:   "a last line that stays put is reported once",
			settle: 2,
			steps: []step{
				{snap: snap("a\n$ ", false), want: []string{"a", "$ "}, initial: true},
				{snap: snap("a\n$ \nError: boom", false)},
				{snap: snap("a\n$ \nError: boom", false), want: []string{"Error: boom"}},
				{snap: snap("a\n$ \nError: boom", false)},
				{snap: snap("a\n$ \nError: boom\n$ ", false)},
			},
		},
		{
			name:   "a settled line that then grows is reported again in full",
			settle: 2,
			steps: []step{
				{snap: snap("a\n$ ", false), want: []string{"a", "$ "}, initial: true},
				{snap: snap("a\n$ \nError", false)},
				{snap: snap("a\n$ \nError", false), want: []string{"Error"}},
				{snap: snap("a\n$ \nError: boom\n$ ", false), want: []string{"Error: boom"}},
			},
		},
		{
			name:   "the same line printed twice is reported twice",
			settle: 2,
			steps: []step{
				{snap: snap("a\n", false), want: []string{"a"}, initial: true},
				{snap: snap("a\nok", false)},
				{snap: snap("a\nok", false), want: []string{"ok"}},
				{snap: snap("a\nok\nok", false)},
				{snap: snap("a\nok\nok", false), want: []string{"ok"}},
			},
		},
		{
			name:   "a blank last line is never reported early",
			settle: 2,
			steps: []step{
				{snap: snap("a\n", false), want: []string{"a"}, initial: true},
				{snap: snap("a\n", false)},
				{snap: snap("a\n", false)},
			},
		},
		{
			name:   "settling can be turned off",
			settle: 0,
			steps: []step{
				{snap: snap("a\n", false), want: []string{"a"}, initial: true},
				{snap: snap("a\nError: boom", false)},
				{snap: snap("a\nError: boom", false)},
				{snap: snap("a\nError: boom\n$ ", false), want: []string{"Error: boom"}},
			},
		},
		{
			name:   "a gap after a settled line doesn't lose anything",
			settle: 2,
			steps: []step{
				{snap: snap("a\nError: boom", false), want: []string{"a", "Error: boom"}, initial: true},
				{snap: snap("fresh\nstart\n$ ", false), want: []string{"fresh", "start"}, gap: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewPaneWatcher(nil, "w1:p1")
			w.SettleReads = tt.settle
			for i, s := range tt.steps {
				got := w.next(s.snap)
				if !slices.Equal(got.Lines, s.want) {
					t.Errorf("step %d: lines = %q, want %q", i, got.Lines, s.want)
				}
				if got.Gap != s.gap {
					t.Errorf("step %d: gap = %v, want %v", i, got.Gap, s.gap)
				}
				if got.Initial != s.initial {
					t.Errorf("step %d: initial = %v, want %v", i, got.Initial, s.initial)
				}
			}
		})
	}
}

// scriptedReader returns one read per call, then the error.
type scriptedReader struct {
	reads []PaneRead
	err   error
	asked []int
}

func (r *scriptedReader) ReadPane(_ context.Context, _ string, lines int) (PaneRead, error) {
	r.asked = append(r.asked, lines)
	if len(r.reads) == 0 {
		return PaneRead{}, r.err
	}
	read := r.reads[0]
	r.reads = r.reads[1:]
	return read, nil
}

func TestPaneWatcherPoll(t *testing.T) {
	gone := &Error{Method: "pane.read", Code: CodePaneNotFound, Message: "pane w1:p1 not found"}
	reader := &scriptedReader{
		reads: []PaneRead{
			{Text: "a\nb"},
			{Text: "a\nb\nc\nd"},
		},
		err: gone,
	}
	w := NewPaneWatcher(reader, "w1:p1")
	ctx := context.Background()

	if d, err := w.Poll(ctx); err != nil || !slices.Equal(d.Lines, []string{"a", "b"}) || !d.Initial {
		t.Fatalf("first poll = %+v, %v", d, err)
	}
	if d, err := w.Poll(ctx); err != nil || !slices.Equal(d.Lines, []string{"c"}) {
		t.Fatalf("second poll = %+v, %v", d, err)
	}
	_, err := w.Poll(ctx)
	if !IsPaneNotFound(err) {
		t.Fatalf("poll after the pane closed: err = %v, want pane not found", err)
	}
	if !errors.Is(err, gone) {
		t.Errorf("err = %v, want it to wrap herdr's error", err)
	}
	if !slices.Equal(reader.asked, []int{DefaultWatchLines, DefaultWatchLines, DefaultWatchLines}) {
		t.Errorf("lines asked for = %v, want %d each time", reader.asked, DefaultWatchLines)
	}
}

func TestPaneWatcherLinesAreTheCallers(t *testing.T) {
	w := NewPaneWatcher(nil, "w1:p1")
	d := w.next(snap("a\nb\n$ ", false))
	d.Lines[0] = "changed"
	got := w.next(snap("a\nb\nc\n$ ", false))
	if !slices.Equal(got.Lines, []string{"c"}) {
		t.Errorf("lines = %q, want the diff unaffected by the caller's edit", got.Lines)
	}
}

func TestPaneWatcherOversizedRead(t *testing.T) {
	reader := &scriptedReader{
		reads: []PaneRead{{Text: "a\nb\n$ "}},
		err:   fmt.Errorf("herdr pane.read: reading reply: %w", ErrTooLarge),
	}
	w := NewPaneWatcher(reader, "w1:p1")
	ctx := context.Background()
	if _, err := w.Poll(ctx); err != nil {
		t.Fatalf("first poll: %v", err)
	}

	d, err := w.Poll(ctx)
	if err != nil {
		t.Fatalf("oversized poll: err = %v, want a gap instead", err)
	}
	if !d.Gap || d.Lines != nil {
		t.Errorf("oversized poll = %+v, want a gap with no lines", d)
	}

	reader.reads = []PaneRead{{Text: "a\nb\n$ \nc\n$ "}}
	if d, err := w.Poll(ctx); err != nil || !slices.Equal(d.Lines, []string{"c"}) {
		t.Errorf("poll after = %+v, %v, want it to diff against the last good read", d, err)
	}
}

func TestPaneWatcherLinesHasAFloor(t *testing.T) {
	tests := []struct {
		lines int
		want  int
	}{
		{lines: -5, want: MinWatchLines},
		{lines: 0, want: MinWatchLines},
		{lines: 1, want: MinWatchLines},
		{lines: MinWatchLines, want: MinWatchLines},
		{lines: 50, want: 50},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.lines), func(t *testing.T) {
			reader := &scriptedReader{reads: []PaneRead{{Text: "a"}}}
			w := NewPaneWatcher(reader, "w1:p1")
			w.Lines = tt.lines
			if _, err := w.Poll(context.Background()); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			if !slices.Equal(reader.asked, []int{tt.want}) {
				t.Errorf("asked for %v lines, want %d", reader.asked, tt.want)
			}
		})
	}
}
