package feed

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// screen collects what Bubble Tea draws, safe to read while it draws.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *screen) contains(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.buf.String(), text)
}

// waitFor polls cond until it's true or five seconds pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPanicStopsEverything makes each part of the feed panic in turn while it
// runs against the fake herdr. Every time, Run has to return the panic as an
// error naming where it happened, and every goroutine the feed started has to
// go.
func TestPanicStopsEverything(t *testing.T) {
	tests := []struct {
		name  string
		where string // the panic point that panics
		// send says the test catches an error and presses enter, for the
		// panic points only a send reaches.
		send bool
		// wantIn is what the error has to say about where it happened.
		wantIn string
	}{
		{name: "tick loop", where: "the tick loop", wantIn: "the tick loop panicked: boom"},
		{name: "sync loop", where: "the sync loop", wantIn: "the sync loop panicked: boom"},
		{name: "watch list loop", where: "the watch list loop", wantIn: "the watch list loop panicked: boom"},
		{name: "config loop", where: "the config loop", wantIn: "the config loop panicked: boom"},
		// Past the config loop's start, a panic is caught inside it and the
		// loop keeps reading until Watch has gone.
		{name: "config forwarding", where: "forwarding config updates", wantIn: "forwarding config updates panicked: boom"},
		{name: "pane events loop", where: "the pane events loop", wantIn: "the pane events loop panicked: boom"},
		{name: "pane loop", where: "the loop for pane " + webPane, wantIn: "the loop for pane " + webPane + " panicked: boom"},
		{name: "agent status loop", where: "the status loop for agent " + agentPane, wantIn: "the status loop for agent " + agentPane + " panicked: boom"},
		{name: "update", where: "update", wantIn: "the feed's update panicked: boom"},
		{name: "view", where: "view", wantIn: "the feed's view panicked: boom"},
		{name: "command", where: "a command", send: true, wantIn: "a command panicked: boom"},
		{name: "send", where: "after prompt", send: true, wantIn: "may or may not have reached"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeHerdr(t)
			before := runtime.NumGoroutine()

			// The update, view and command panic points are passed on every
			// message, draw or command, so they only panic once the test is
			// ready: the loops all running, or enter about to be pressed.
			ready := make(chan struct{})
			hook := func(where string) {
				if where != tt.where {
					return
				}
				if where == "update" || where == "view" || where == "a command" {
					select {
					case <-ready:
					default:
						return
					}
				}
				panic("boom")
			}
			ctx := context.WithValue(context.Background(), panicKey{}, hook)

			in, keys := io.Pipe()
			defer keys.Close()
			out := &screen{}
			done := make(chan error, 1)
			go func() {
				done <- Run(ctx, Options{
					Client:    f.client(),
					StateDir:  t.TempDir(),
					ConfigDir: t.TempDir(),
					Watch:     []string{webPane},
					Self:      feedPane,
					Every:     Timings{Poll: 20 * time.Millisecond, Sync: 20 * time.Millisecond, Tick: 20 * time.Millisecond, List: 20 * time.Millisecond},
					Program:   []tea.ProgramOption{tea.WithInput(in), tea.WithOutput(out), tea.WithoutSignalHandler()},
				})
			}()

			if tt.send {
				// Catch an error in the watched pane, select it and press
				// enter. The agent is idle in the same tab, so one enter sends.
				waitFor(t, "the first read", func() bool { return f.reads.Load() >= 2 })
				f.mu.Lock()
				f.text = "ready\nTypeError: Cannot read properties of undefined (reading 'map')\n    at NoticeList (app/notices/page.tsx:14:22)"
				f.mu.Unlock()
				waitFor(t, "the entry to show", func() bool { return out.contains("TypeError") })
				close(ready)
				for _, k := range []string{"j", "\r"} {
					if _, err := keys.Write([]byte(k)); err != nil {
						t.Fatal(err)
					}
				}
			} else if tt.where == "update" || tt.where == "view" {
				waitFor(t, "the loops to run", func() bool { return f.reads.Load() >= 3 })
				close(ready)
				// A key makes sure there's a message and a draw after the
				// panic points are armed.
				if _, err := keys.Write([]byte("j")); err != nil {
					t.Fatal(err)
				}
			}
			// A loop's panic point is where it starts, so it needs nothing.

			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the panic didn't stop the feed")
			}
			_ = keys.Close()

			var pe *panicError
			if !errors.As(err, &pe) {
				t.Fatalf("Run returned %v, want a panic error", err)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("the error doesn't say %q:\n%v", tt.wantIn, err)
			}
			if !strings.Contains(err.Error(), "goroutine ") {
				t.Errorf("the error has no stack trace:\n%v", err)
			}
			prompts := f.promptCalls()
			switch {
			case tt.where == "after prompt" && len(prompts) != 1:
				t.Errorf("got %d prompts, want exactly 1", len(prompts))
			case tt.where != "after prompt" && len(prompts) != 0:
				t.Errorf("got %d prompts, want none", len(prompts))
			}

			deadline := time.Now().Add(5 * time.Second)
			for runtime.NumGoroutine() > before {
				if time.Now().After(deadline) {
					buf := make([]byte, 1<<20)
					n := runtime.Stack(buf, true)
					t.Fatalf("%d goroutines before, %d after the panic:\n%s", before, runtime.NumGoroutine(), buf[:n])
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// TestPanicDuringASendSaysSo makes the update panic while a send is in flight.
// The send's result is dropped with the model, so Run's error has to say a
// send may have gone through.
func TestPanicDuringASendSaysSo(t *testing.T) {
	f := newFakeHerdr(t)
	inFlight := make(chan struct{})
	failing := make(chan struct{})
	var once sync.Once
	hook := func(where string) {
		switch where {
		case "a command":
			// Hold the target check until the update has panicked.
			once.Do(func() { close(inFlight) })
			<-failing
		case "update":
			select {
			case <-inFlight:
				close(failing)
				panic("boom")
			default:
			}
		}
	}
	ctx := context.WithValue(context.Background(), panicKey{}, hook)
	in, keys := io.Pipe()
	defer keys.Close()
	out := &screen{}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Client:  f.client(),
			Watch:   []string{webPane},
			Self:    feedPane,
			Every:   Timings{Poll: 20 * time.Millisecond, Sync: 20 * time.Millisecond, Tick: 20 * time.Millisecond, List: 20 * time.Millisecond},
			Program: []tea.ProgramOption{tea.WithInput(in), tea.WithOutput(out), tea.WithoutSignalHandler()},
		})
	}()
	waitFor(t, "the first read", func() bool { return f.reads.Load() >= 2 })
	f.mu.Lock()
	f.text = "ready\nTypeError: Cannot read properties of undefined (reading 'map')\n    at NoticeList (app/notices/page.tsx:14:22)"
	f.mu.Unlock()
	waitFor(t, "the entry to show", func() bool { return out.contains("TypeError") })
	for _, k := range []string{"j", "\r"} {
		if _, err := keys.Write([]byte(k)); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the panic didn't stop the feed")
	}
	if err == nil || !strings.Contains(err.Error(), "the feed's update panicked: boom") || !strings.Contains(err.Error(), "1 send(s) were under way") {
		t.Errorf("Run returned %v, want the panic and the send in flight", err)
	}
}

// TestPanicInSendFlagsMaybeSent drives the send by hand so it can look at the
// model afterwards. A panic after agent.prompt may come after herdr typed the
// prompt, so the entry has to be flagged as maybe sent and the feed stopped
// with an error that says so.
func TestPanicInSendFlagsMaybeSent(t *testing.T) {
	h := newHarness(t)
	h.model.ctx = context.WithValue(h.model.ctx, panicKey{}, func(where string) {
		if where == "after prompt" {
			panic("boom")
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &guarded{model: h.model, faults: &faults{ctx: ctx, cancel: cancel}}

	en := h.catchError()
	_, cmd := g.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for cmd != nil {
		_, cmd = g.Update(cmd())
	}

	if got := len(h.herdr.promptCalls()); got != 1 {
		t.Fatalf("got %d prompts, want 1", got)
	}
	if h.model.maybeSent[en.ID] == "" {
		t.Errorf("the entry isn't flagged as maybe sent")
	}
	if h.model.sending[en.ID] {
		t.Errorf("the entry is still marked as sending")
	}
	err := g.faults.err()
	if err == nil || !strings.Contains(err.Error(), "may or may not have reached") || !strings.Contains(err.Error(), "sending the prompt panicked: boom") {
		t.Errorf("faults has %v, want the panic and that it may have been sent", err)
	}
	if ctx.Err() == nil {
		t.Errorf("the root context wasn't cancelled")
	}
	// Once failed, the model is left alone.
	if _, cmd := g.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Errorf("the update gave a command after the feed failed")
	}
	if v := g.View(); v != "" {
		t.Errorf("the view drew %q after the feed failed", v)
	}
}

// TestFaults checks the shared catch: the first panic is kept, the root is
// cancelled and a guarded command gives back no message.
func TestFaults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &faults{ctx: ctx, cancel: cancel}

	if f.err() != nil {
		t.Fatalf("a new faults has an error: %v", f.err())
	}
	if msg := f.guard("a command", func() tea.Msg { panic("first") })(); msg != nil {
		t.Errorf("a command that panicked gave back %v", msg)
	}
	func() {
		defer f.catch("the second")
		panic(errors.New("second"))
	}()

	err := f.err()
	var pe *panicError
	if !errors.As(err, &pe) || pe.where != "a command" || pe.value != "first" {
		t.Errorf("got %v, want the first panic", err)
	}
	if ctx.Err() == nil {
		t.Errorf("the root context wasn't cancelled")
	}
	if f.guard("nothing", nil) != nil {
		t.Errorf("guarding no command made one")
	}
	// The error reaches the terminal, so pane text in a panic can't drive it.
	if text := newPanicError("the tick loop", "\x1b]0;pwned\x07\x1b[2J").Error(); strings.ContainsAny(text, "\x1b\x07") || !strings.Contains(text, `\x1b`) {
		t.Errorf("the panic error didn't escape its control characters: %q", text)
	}

	// A panic carrying a herdr refusal mustn't read as one: the model takes a
	// refusal to mean nothing was typed.
	var refused *herdr.Error
	if errors.As(newPanicError("sending the prompt", &herdr.Error{Code: "agent_blocked"}), &refused) {
		t.Errorf("a panic error unwrapped to a herdr refusal")
	}
}
