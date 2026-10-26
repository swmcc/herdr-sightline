package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/engine"
)

// hugeLine is the most herdr hands over in one read.
const hugeLine = 16 << 20

// presetOptions are options with every preset, which is what makes a huge line
// expensive to match.
func presetOptions(t testing.TB) engine.Options {
	t.Helper()
	presets, err := engine.Presets(engine.PresetNames()...)
	if err != nil {
		t.Fatal(err)
	}
	opts := options()
	opts.Matchers = presets
	return opts
}

// repeatTo repeats s until it's n bytes long or just over.
func repeatTo(s string, n int) string {
	return strings.Repeat(s, (n+len(s)-1)/len(s))
}

// One huge line used to freeze the feed for about half a minute, because every
// preset read all of it (#65). Only the head of a line is matched now. The bound
// is generous so a slow CI runner doesn't fail it. On a laptop each line takes
// a few hundred milliseconds.
func TestHugeLineIsQuick(t *testing.T) {
	size, limit := hugeLine, 2*time.Second
	if raceEnabled {
		// The race detector slows the scrubber, which still reads every byte,
		// by about fifteen times. A quarter of the size and five times the
		// time keeps the suite quick and still fails by miles without the fix:
		// a 4 MiB line took about 9s then, without the race detector.
		size, limit = hugeLine/4, 10*time.Second
	}
	tests := []struct {
		name    string
		line    string
		entries int
	}{
		{"plain text", repeatTo("a", size), 0},
		{"an error at the start", "Error: boom " + repeatTo("a", size), 1},
		{"errors all the way along", repeatTo("TypeError: x at Foo.bar (a.js:1:1) ", size), 1},
		{"wide characters", repeatTo("é😀", size), 0},
		{"redrawn over and over", repeatTo("Error: boom\r", size), 1},
		{"spaces", repeatTo(" ", size) + "x", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := engine.New(presetOptions(t))
			e.Watch(engine.Source{ID: web})
			now := time.Unix(0, 0)
			begin := time.Now()
			e.Feed(web, []string{tt.line}, now)
			// Filing the entry and fingerprinting it count too.
			e.Tick(now.Add(time.Second))
			took := time.Since(begin)
			t.Logf("%d MiB line took %v", len(tt.line)>>20, took)
			if took > limit {
				t.Fatalf("feeding a %d byte line took %v, want under %v", len(tt.line), took, limit)
			}
			if got := len(e.Entries()); got != tt.entries {
				t.Fatalf("%d entries, want %d", got, tt.entries)
			}
		})
	}
}

// Matching only looks at the head of a line, the same 4 KiB the window keeps.
// An error past it isn't caught, and neither is an ignore rule past it.
func TestOnlyTheHeadOfALineIsMatched(t *testing.T) {
	pad := strings.Repeat("x", 5000)
	tests := []struct {
		name    string
		line    string
		entries int
	}{
		{"error in the head", "Error: boom " + pad, 1},
		{"error just inside the head", strings.Repeat("x", 4096-len("Error")) + "Error" + pad, 1},
		{"error straddling the end of the head", strings.Repeat("x", 4096-len("Err")) + "Error" + pad, 0},
		{"error past the head", pad + " Error: boom", 0},
		{"ignore rule past the head", "Error: boom " + pad + " DeprecationWarning", 1},
		{"ignore rule in the head", "Error: DeprecationWarning " + pad, 0},
		// The head is taken after the scrubber has read the whole line, so
		// escapes and strings before an error don't push it out of the head.
		{"escapes before the error", strings.Repeat("\x1b[0m", 5000) + "Error: boom", 1},
		{"a string before the error", "\x1b]0;" + pad + "\x07Error: boom", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAdapter(t)
			a.Print(web, tt.line)
			a.Advance(time.Second)
			if got := len(a.Engine.Entries()); got != tt.entries {
				t.Fatalf("%d entries, want %d", got, tt.entries)
			}
		})
	}
}

// A pattern anchored at the end of the line mustn't match where the head was
// cut. Each line here fills the head exactly, so without the care in scanned
// the head would end on a match.
func TestCutHeadIsntTheEndOfTheLine(t *testing.T) {
	notFound := "zsh: command not found: "
	tests := []struct {
		name    string
		line    string
		entries int
	}{
		{"short line still matches", notFound + "foo", 1},
		{"one word left at the cut", notFound + strings.Repeat("x", 4096-len(notFound)) + " more", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := engine.New(presetOptions(t))
			e.Watch(engine.Source{ID: web})
			now := time.Unix(0, 0)
			e.Feed(web, []string{tt.line}, now)
			e.Tick(now.Add(time.Second))
			if got := len(e.Entries()); got != tt.entries {
				t.Fatalf("%d entries, want %d: %+v", got, tt.entries, e.Entries())
			}
		})
	}
}

// BenchmarkHugeLine feeds one 16 MiB line, with every preset on.
func BenchmarkHugeLine(b *testing.B) {
	line := repeatTo("a", hugeLine)
	opts := presetOptions(b)
	now := time.Unix(0, 0)
	b.SetBytes(int64(len(line)))
	for b.Loop() {
		e := engine.New(opts)
		e.Watch(engine.Source{ID: web})
		e.Feed(web, []string{line}, now)
	}
}
