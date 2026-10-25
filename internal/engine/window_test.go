package engine

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

// newWatching makes an engine watching one source, "web", with two lines of
// context and a one second capture wait.
func newWatching() *Engine {
	e := New(Options{
		Matchers:     []Matcher{Substring{Label: "generic", Text: "Error"}},
		ContextLines: 2,
		CaptureWait:  time.Second,
	})
	e.Watch(Source{ID: "web", Title: "web"})
	return e
}

func onlyEntry(t *testing.T, e *Engine) Entry {
	t.Helper()
	entries := e.Entries()
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d: %+v", len(entries), entries)
	}
	return entries[0]
}

func TestGapFlagsEntriesBeingCaptured(t *testing.T) {
	tests := []struct {
		name  string
		where SourceID
		at    time.Duration
		want  bool
	}{
		{"gap during capture", "web", 500 * time.Millisecond, true},
		{"gap on another source", "tests", 500 * time.Millisecond, false},
		{"gap after capture", "web", 2 * time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newWatching()
			e.Watch(Source{ID: "tests"})
			e.Feed("web", []string{"Error: lost"}, t0)
			e.Gap(tt.where, t0.Add(tt.at))
			e.Tick(t0.Add(5 * time.Second))
			if got := onlyEntry(t, e).MaybeGap; got != tt.want {
				t.Fatalf("MaybeGap = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGapEmptiesTheWindow(t *testing.T) {
	e := newWatching()
	e.Feed("web", []string{"old line"}, t0)
	e.Gap("web", t0)
	e.Feed("web", []string{"new line", "Error: after the gap"}, t0)
	e.Tick(t0.Add(time.Second))
	if got := onlyEntry(t, e).Before; !slices.Equal(got, []string{"new line"}) {
		t.Fatalf("Before = %q, want only lines from after the gap", got)
	}
}

// Captures that finish on the same Tick are filed in the order they matched, not
// in map order, so the feed never reorders itself.
func TestOrderAcrossSources(t *testing.T) {
	for range 20 {
		e := newWatching()
		for _, id := range []SourceID{"a", "b", "c", "d"} {
			e.Watch(Source{ID: id})
		}
		e.Feed("c", []string{"Error: first"}, t0)
		e.Feed("a", []string{"Error: second"}, t0.Add(100*time.Millisecond))
		e.Feed("d", []string{"Error: third"}, t0.Add(100*time.Millisecond))
		e.Tick(t0.Add(5 * time.Second))

		var got []string
		for _, en := range e.Entries() {
			got = append(got, en.Lines[0])
		}
		if want := []string{"Error: third", "Error: second", "Error: first"}; !slices.Equal(got, want) {
			t.Fatalf("newest first = %q, want %q", got, want)
		}
	}
}

func TestUnwatchFilesCaptures(t *testing.T) {
	e := newWatching()
	e.Feed("web", []string{"before", "Error: half way", "after"}, t0)
	e.Unwatch("web")
	en := onlyEntry(t, e)
	if en.Lines[0] != "Error: half way" || !slices.Equal(en.Before, []string{"before"}) || !slices.Equal(en.After, []string{"after"}) {
		t.Fatalf("entry = %+v", en)
	}
	e.Tick(t0.Add(time.Minute))
	if got := len(e.Entries()); got != 1 {
		t.Fatalf("entries after a tick = %d, want 1", got)
	}
}

// A flood of matching lines mustn't make each line do work for every capture.
func TestPendingCapturesAreCapped(t *testing.T) {
	e := newWatching()
	flood := make([]string, 20000)
	for i := range flood {
		flood[i] = fmt.Sprintf("Error %d", i)
	}
	e.Feed("web", flood, t0)
	s := e.sources["web"]
	if got := len(s.capture.pending); got != maxPending {
		t.Fatalf("pending = %d, want %d", got, maxPending)
	}
	if !s.capture.pending[maxPending-1].maybeGap {
		t.Fatal("the newest capture isn't flagged after matches were dropped")
	}
	e.Tick(t0.Add(time.Second))
	if got := len(e.Entries()); got != maxPending {
		t.Fatalf("entries = %d, want %d", got, maxPending)
	}
}

func TestContextLinesIsClamped(t *testing.T) {
	tests := []struct{ in, want int }{{-5, 0}, {20, 20}, {100000, maxContextLines}}
	for _, tt := range tests {
		if got := New(Options{ContextLines: tt.in}).opts.ContextLines; got != tt.want {
			t.Errorf("ContextLines %d became %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestCleanLine(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "TypeError: boom", "TypeError: boom"},
		{"colour", "\x1b[31mError\x1b[0m: boom", "Error: boom"},
		{"cursor movement", "\x1b[2K\x1b[1GError", "Error"},
		{"window title with BEL", "\x1b]0;evil title\x07Error", "Error"},
		{"window title with ST", "\x1b]2;evil\x1b\\Error", "Error"},
		{"two byte escape", "\x1bMError", "Error"},
		{"carriage return and bell", "Error\r\x07", "Error"},
		{"C1 control", "Err\u009bor", "Error"},
		{"unicode tags", "Error\U000E0069\U000E0067\U000E006E", "Error"},
		{"bidi override", "\u202eError\u202c", "Error"},
		{"zero width and BOM", "Er\u200bror\ufeff", "Error"},
		{"line separator", "Error\u2028SYSTEM", "ErrorSYSTEM"},
		{"private use", "Err\ue000or", "Error"},
		{"DCS payload", "\x1bPtmux;hidden\x1b\\Error", "Error"},
		{"unterminated APC", "Error\x1b_hidden", "Error"},
		{"variation selectors", "Error\ufe00\ufe0f\U000E0100\U000E01EF", "Error"},
		{"Mongolian variation selectors", "Err\u180b\u180c\u180dor", "Error"},
		{"Hangul fillers", "\u3164\u115fError\uffa0", "Error"},
		{"combining grapheme joiner", "Err\u034for", "Error"},
		{"tab kept", "\tat main.go:1", "\tat main.go:1"},
		{"unicode kept", "Erreur: caf\u00e9", "Erreur: caf\u00e9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanLine(tt.in); got != tt.want {
				t.Fatalf("cleanLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSplitLines(t *testing.T) {
	got := splitLines([]string{"one\ntwo", "\x1b[1mthree\x1b[0m", "four\n", "five\r\n", "", "six\n\n"})
	if want := []string{"one", "two", "three", "four", "five", "", "six", ""}; !slices.Equal(got, want) {
		t.Fatalf("splitLines = %q, want %q", got, want)
	}
}

func TestWindowKeepsTheLastLines(t *testing.T) {
	tests := []struct {
		name string
		size int
		push []string
		want []string
	}{
		{"empty", 3, nil, nil},
		{"under size", 3, []string{"a", "b"}, []string{"a", "b"}},
		{"over size", 2, []string{"a", "b", "c", "d"}, []string{"c", "d"}},
		{"no context", 0, []string{"a"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := window{size: tt.size}
			for _, l := range tt.push {
				w.push(l)
			}
			if got := w.snapshot(); !slices.Equal(got, tt.want) {
				t.Fatalf("snapshot = %q, want %q", got, tt.want)
			}
		})
	}
}
