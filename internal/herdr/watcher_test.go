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
		name    string
		prev    Snapshot
		curr    Snapshot
		want    []string
		wantGap bool
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
			name: "a window of nothing but one repeated line can't show new copies",
			prev: snap(strings.Repeat(".\n", 9)+".", true),
			curr: snap(strings.Repeat(".\n", 9)+".", true),
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
			name:    "a single matching line isn't trusted",
			prev:    snap("build\nok\n$ \nlater", true),
			curr:    snap("$ \nsomething\nelse\nnow", true),
			want:    []string{"$ ", "something", "else"},
			wantGap: true,
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
				{snap: snap("a\nb\n$ ", false), want: []string{"a", "b"}, initial: true},
			},
		},
		{
			name:   "a growing last line waits for the line after it",
			settle: 2,
			steps: []step{
				{snap: snap("a\nTypeEr", false), want: []string{"a"}, initial: true},
				{snap: snap("a\nTypeError: x", false)},
				{snap: snap("a\nTypeError: x is undefined\n    at main", false), want: []string{"TypeError: x is undefined"}},
			},
		},
		{
			name:   "a last line that stays put is reported once",
			settle: 2,
			steps: []step{
				{snap: snap("a\nError: boom", false), want: []string{"a"}, initial: true},
				{snap: snap("a\nError: boom", false), want: []string{"Error: boom"}},
				{snap: snap("a\nError: boom", false)},
				{snap: snap("a\nError: boom\n$ ", false)},
			},
		},
		{
			name:   "a settled line that then grows is reported again in full",
			settle: 2,
			steps: []step{
				{snap: snap("a\nError", false), want: []string{"a"}, initial: true},
				{snap: snap("a\nError", false), want: []string{"Error"}},
				{snap: snap("a\nError: boom\n$ ", false), want: []string{"Error: boom"}},
			},
		},
		{
			name:   "the same line printed twice is reported twice",
			settle: 2,
			steps: []step{
				{snap: snap("a\nok", false), want: []string{"a"}, initial: true},
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
				{snap: snap("a\nError: boom", false), want: []string{"a"}, initial: true},
				{snap: snap("a\nError: boom", false)},
				{snap: snap("a\nError: boom", false)},
				{snap: snap("a\nError: boom\n$ ", false), want: []string{"Error: boom"}},
			},
		},
		{
			name:   "a gap after a settled line doesn't lose anything",
			settle: 2,
			steps: []step{
				{snap: snap("a\nError: boom", false), want: []string{"a"}, initial: true},
				{snap: snap("a\nError: boom", false), want: []string{"Error: boom"}},
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

	if d, err := w.Poll(ctx); err != nil || !slices.Equal(d.Lines, []string{"a"}) || !d.Initial {
		t.Fatalf("first poll = %+v, %v", d, err)
	}
	if d, err := w.Poll(ctx); err != nil || !slices.Equal(d.Lines, []string{"b", "c"}) {
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
