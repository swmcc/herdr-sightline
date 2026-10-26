package injection

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/herdr"
	"github.com/swmcc/herdr-sightline/internal/herdr/herdrtest"
)

// The adapter's side of the bargain, from the injection reviews of #40 (see the
// comment "Edges to cover" on #15). The engine's scrubber can only hold up if
// it's handed whole lines of valid UTF-8 and told about gaps the right way.

// scriptReader returns one read after another, then the last one for ever.
type scriptReader struct {
	texts []string
}

func (r *scriptReader) ReadPane(_ context.Context, id string, _ int) (herdr.PaneRead, error) {
	text := r.texts[0]
	if len(r.texts) > 1 {
		r.texts = r.texts[1:]
	}
	return herdr.PaneRead{PaneID: id, Text: text}, nil
}

// A two byte C1 split across two Feed elements becomes two U+FFFD, and the
// string it opens is never opened, so its payload shows. The engine can't tell,
// which is why the adapter must only ever hand it whole lines.
func TestSplitC1LeaksWhenTheContractIsBroken(t *testing.T) {
	e := engine.New(options(t))
	e.Watch(engine.Source{ID: paneID})
	e.Feed(paneID, []string{"Error: A\xc2", "\x9dPAYLOAD\x07 B"}, start)
	e.Tick(start.Add(time.Minute))
	if !strings.Contains(messages(t, e), "PAYLOAD") {
		t.Fatal("the split C1 hid its payload after all, so this test no longer shows why the contract matters")
	}
}

// The watcher holds a half written last line back, and when it reports one
// early (after SettleReads) it reports the whole line again once it's done,
// never the rest of it on its own. So a C1 split across two reads reaches the
// engine whole the second time, and its string stays hidden.
func TestWatcherHandsOverWholeLines(t *testing.T) {
	reads := []string{
		"GET / 200\nError: A\xc2",
		"GET / 200\nError: A\xc2",
		"GET / 200\nError: A\xc2",
		"GET / 200\nError: A\xc2\x9dPAYLOAD\x07 B\nnext\n",
	}
	w := herdr.NewPaneWatcher(&scriptReader{texts: reads}, paneID)
	w.SettleReads = 1
	e := engine.New(options(t))
	e.Watch(engine.Source{ID: paneID})

	whole := map[string]bool{}
	for _, r := range reads {
		for _, l := range strings.Split(r, "\n") {
			whole[l] = true
		}
	}
	now := start
	for range len(reads) + 1 {
		now = now.Add(time.Second)
		d, err := w.Poll(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range d.Lines {
			if !whole[l] {
				t.Fatalf("watcher handed over %q, which isn't a whole line of any read", l)
			}
		}
		e.Feed(paneID, d.Lines, now)
	}
	e.Tick(now.Add(time.Minute))
	if msgs := messages(t, e); strings.Contains(msgs, "PAYLOAD") {
		t.Fatalf("the payload of a C1 split across reads reached a message:\n%s", msgs)
	}
}

// herdr's text arrives as JSON, which can't carry invalid UTF-8: the client
// decodes any that herdr sends as U+FFFD. So the lines the watcher hands over
// are always valid UTF-8, even if herdr's own text wasn't.
func TestReadPaneIsValidUTF8(t *testing.T) {
	srv := herdrtest.New(t)
	srv.Handle("pane.read", func(req herdrtest.Request) (any, *herdrtest.ErrorBody) {
		// Raw bytes in the JSON string: a lone lead byte, a raw C1 and a cut
		// three byte character. The \n are JSON escapes.
		text := `Error: A` + "\xc2" + `\n` + "\x9d" + `B ` + "\xe2\x80" + `\nnext`
		return herdrtest.Raw(fmt.Sprintf(`{"id":%q,"result":{"type":"pane_read","read":{"pane_id":"w1:p3","text":"%s","truncated":false}}}`+"\n", req.ID, text)), nil
	})
	c := herdr.NewClient(herdr.UnixDialer(srv.Path))
	read, err := c.ReadPane(context.Background(), paneID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(read.Text) {
		t.Fatalf("ReadPane returned invalid UTF-8: %q", read.Text)
	}
	if want := "Error: A�\n�B ��\nnext"; read.Text != want {
		t.Fatalf("ReadPane text = %q, want %q", read.Text, want)
	}
}

// After a reconnect the adapter must call Gap, never Unwatch then Watch. #9's
// feed owns the reconnect, so this is the engine side of that contract. With Gap
// the error being captured when the connection dropped is flagged, and the
// modes that stop redraws being followed are kept. Unwatch files it straight
// away as if it were complete. It also forgets the modes, which this test
// doesn't check because unhide catches the decoy either way.
func TestReconnectCallsGap(t *testing.T) {
	reconnects := []struct {
		name      string
		reconnect func(e *engine.Engine, now time.Time)
		wantGap   bool
	}{
		{"Gap", func(e *engine.Engine, now time.Time) { e.Gap(paneID, now) }, true},
		{"Unwatch then Watch", func(e *engine.Engine, now time.Time) {
			e.Unwatch(paneID)
			e.Watch(engine.Source{ID: paneID})
		}, false},
	}
	for _, rc := range reconnects {
		t.Run(rc.name, func(t *testing.T) {
			e := engine.New(options(t))
			e.Watch(engine.Source{ID: paneID})
			// Insert mode lasts beyond its line, so redraws can't be trusted
			// until the terminal is reset.
			e.Feed(paneID, []string{"\x1b[4h", "Error: dropped mid trace", "    at first (a.js:1:1)"}, start)
			rc.reconnect(e, start.Add(100*time.Millisecond))
			e.Feed(paneID, []string{"    at second (a.js:2:2)", "Error: real\rok"}, start.Add(200*time.Millisecond))
			e.Tick(start.Add(time.Minute))

			var dropped, redrawn *engine.Entry
			for _, en := range e.Entries() {
				switch {
				case strings.HasPrefix(en.Lines[0], "Error: dropped"):
					dropped = &en
				case strings.HasPrefix(en.Lines[0], "Error: real") || en.Lines[0] == "ok":
					redrawn = &en
				}
			}
			if dropped == nil || dropped.MaybeGap != rc.wantGap {
				t.Fatalf("entry caught before the reconnect = %+v, want MaybeGap %v", dropped, rc.wantGap)
			}
			if redrawn == nil {
				t.Fatal("the error after the reconnect wasn't caught")
			}
			// With the mode kept, the line isn't redrawn at all: both texts are
			// kept as printed, and nothing was drawn over to flag.
			if rc.wantGap && (redrawn.Lines[0] != "Error: realok" || redrawn.MaybeGap) {
				t.Fatalf("after Gap the line = %q MaybeGap %v, want it kept as printed and unflagged", redrawn.Lines[0], redrawn.MaybeGap)
			}
		})
	}
}

// What herdr 0.9.1 does with C1 controls, checked live for #15 by printing raw
// bytes into a throwaway tab and reading it back with `herdr pane read`:
//
//   - Raw 8-bit C1 bytes (0x80 to 0x9f, invalid UTF-8) show as U+FFFD and aren't
//     acted on, so the text after them stays visible. The scrubber does the same.
//   - UTF-8 C1 controls (U+0080 to U+009F) are dropped but not acted on: the
//     text after U+009D or U+0090 is visible, U+009B doesn't start a CSI and
//     U+0085 doesn't start a new line. The scrubber acts on them, so it hides
//     more than herdr shows, and the shadow flags what follows.
//   - ESC strings are hidden, and an unterminated OSC hides the lines after it
//     until a BEL, as the scrubber assumes.
//   - Tag characters (U+E0041) come through pane.read. Zero width spaces, bidi
//     controls and variation selectors don't.
//
// Either way, herdr has already drawn the text it hands Sightline, so in
// practice no escape or C1 reaches the engine at all.
func TestRawC1IsShownLikeHerdrShowsIt(t *testing.T) {
	e := engine.New(options(t))
	e.Watch(engine.Source{ID: paneID})
	e.Feed(paneID, []string{"Error: A\x9dSHOWN\x07B"}, start)
	e.Tick(start.Add(time.Minute))
	entries := e.Entries()
	if len(entries) != 1 || entries[0].Lines[0] != "Error: A�SHOWNB" {
		t.Fatalf("entries = %+v, want the raw C1 shown as U+FFFD with its text", entries)
	}
}

// An escape left open at the end of a line eats the start of the next, as a
// terminal does (herdr showed "rror: anchored" for this). So a pattern anchored
// with ^ can miss it. The person sees the same thing, so it's not hidden, but
// the scrubber flags it as a gap so the next error says its context may have a
// hole in it. Through herdr the escape is already gone and there's no flag,
// which is fine: herdr's text is what the person sees.
func TestOpenEscapeEatsTheNextLinesStart(t *testing.T) {
	anchored := engine.Regexp("anchored", regexp.MustCompile(`^Error: `))
	e := engine.New(engine.Options{Matchers: []engine.Matcher{anchored}, ContextLines: 5, CaptureWait: time.Second})
	e.Watch(engine.Source{ID: paneID})
	e.Feed(paneID, []string{"GET / 200 \x1b[31", "Error: anchored", "Error: next"}, start)
	e.Tick(start.Add(time.Minute))
	entries := e.Entries()
	if len(entries) != 1 || entries[0].Lines[0] != "Error: next" {
		t.Fatalf("entries = %+v, want only the next error caught", entries)
	}
	if !entries[0].MaybeGap {
		t.Fatal("the error after the eaten line isn't flagged MaybeGap")
	}
	if !slices.Contains(entries[0].Before, "rror: anchored") {
		t.Fatalf("context = %q, want the eaten line as a terminal shows it", entries[0].Before)
	}
}

// messages builds every entry's message, checks each with the parser and
// returns them joined.
func messages(t *testing.T, e *engine.Engine) string {
	t.Helper()
	var all []string
	for _, en := range e.Entries() {
		msg, err := e.Message(en.ID)
		if err != nil {
			t.Fatal(err)
		}
		parse(t, msg, en)
		all = append(all, msg)
	}
	return strings.Join(all, "\n")
}
