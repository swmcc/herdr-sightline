package feed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/engine"
	"github.com/swmcc/herdr-sightline/internal/herdr"
	"github.com/swmcc/herdr-sightline/internal/herdr/herdrtest"
	"github.com/swmcc/herdr-sightline/internal/watchlist"
)

// The panes in the fake herdr: a watched dev server and an agent beside it in
// tab 2, plus the feed itself.
const (
	webPane    = "w1:p1"
	agentPane  = "w1:p2"
	otherAgent = "w1:p3"
	feedPane   = "w1:p9"
)

// clock is a fake clock that's safe to read from the loops.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeHerdr is herdrtest with the panes above and a record of every prompt.
type fakeHerdr struct {
	srv *herdrtest.Server

	mu      sync.Mutex
	status  map[string]herdr.AgentStatus // agent pane to its status
	seq     map[string]uint64
	kind    map[string]string // agent pane to its kind, claude if not set
	term    map[string]string // agent pane to its terminal id, if it moved
	text    string            // what pane.read returns for the watched pane
	prompts []promptCall
	reads   atomic.Int64
}

type promptCall struct {
	Target string `json:"target"`
	Text   string `json:"text"`
}

func newFakeHerdr(t *testing.T) *fakeHerdr {
	t.Helper()
	f := &fakeHerdr{
		srv:    herdrtest.New(t),
		status: map[string]herdr.AgentStatus{agentPane: herdr.AgentIdle},
		seq:    map[string]uint64{agentPane: 10},
		kind:   map[string]string{},
		term:   map[string]string{},
		text:   "ready",
	}
	f.srv.Handle("pane.list", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		f.mu.Lock()
		defer f.mu.Unlock()
		panes := []map[string]any{
			{"pane_id": webPane, "workspace_id": "w1", "tab_id": "w1:t2", "label": "web", "terminal_title_stripped": "next dev"},
			{"pane_id": feedPane, "workspace_id": "w1", "tab_id": "w1:t2"},
		}
		for pane, st := range f.status {
			tab := "w1:t2"
			if pane == otherAgent {
				tab = "w1:t3"
			}
			panes = append(panes, map[string]any{"pane_id": pane, "terminal_id": f.terminalOf(pane), "workspace_id": "w1", "tab_id": tab, "agent": f.kindOf(pane), "agent_status": st})
		}
		return map[string]any{"type": "pane_list", "panes": panes}, nil
	})
	f.srv.Handle("agent.list", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var agents []map[string]any
		for pane, st := range f.status {
			tab := "w1:t2"
			if pane == otherAgent {
				tab = "w1:t3"
			}
			agents = append(agents, map[string]any{"pane_id": pane, "workspace_id": "w1", "tab_id": tab, "agent": f.kindOf(pane), "agent_status": st, "state_change_seq": f.seq[pane]})
		}
		return map[string]any{"type": "agent_list", "agents": agents}, nil
	})
	f.srv.Handle("tab.list", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		return map[string]any{"type": "tab_list", "tabs": []map[string]any{
			{"tab_id": "w1:t2", "workspace_id": "w1", "number": 2},
			{"tab_id": "w1:t3", "workspace_id": "w1", "number": 3},
		}}, nil
	})
	f.srv.Handle("pane.read", func(req herdrtest.Request) (any, *herdrtest.ErrorBody) {
		f.reads.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		var p struct {
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.PaneID != webPane {
			return nil, &herdrtest.ErrorBody{Code: herdr.CodePaneNotFound, Message: "no pane " + p.PaneID}
		}
		return map[string]any{"type": "pane_read", "read": map[string]any{"pane_id": p.PaneID, "text": f.text}}, nil
	})
	f.srv.Handle("agent.prompt", func(req herdrtest.Request) (any, *herdrtest.ErrorBody) {
		var p promptCall
		_ = json.Unmarshal(req.Params, &p)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.prompts = append(f.prompts, p)
		return map[string]any{"type": "agent_prompted"}, nil
	})
	return f
}

func (f *fakeHerdr) setStatus(pane string, st herdr.AgentStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[pane] = st
	f.seq[pane]++
}

// kindOf is the kind of agent in pane. The caller holds f.mu.
func (f *fakeHerdr) kindOf(pane string) string {
	if k, ok := f.kind[pane]; ok {
		return k
	}
	return "claude"
}

// terminalOf is the terminal id of pane, which follows it when it moves. The
// caller holds f.mu.
func (f *fakeHerdr) terminalOf(pane string) string {
	if t, ok := f.term[pane]; ok {
		return t
	}
	return "term-" + pane
}

// movePane gives an agent's pane a new id, as moving it to another workspace
// does, without sending the event.
func (f *fakeHerdr) movePane(from, to string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[to], f.seq[to], f.term[to] = f.status[from], f.seq[from], f.terminalOf(from)
	if k, ok := f.kind[from]; ok {
		f.kind[to] = k
	}
	delete(f.status, from)
	delete(f.seq, from)
}

// setKind puts another kind of agent in pane.
func (f *fakeHerdr) setKind(pane, kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kind[pane] = kind
	f.seq[pane]++
}

// removeAgent closes an agent's pane.
func (f *fakeHerdr) removeAgent(pane string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.status, pane)
	delete(f.seq, pane)
}

func (f *fakeHerdr) promptCalls() []promptCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]promptCall(nil), f.prompts...)
}

func (f *fakeHerdr) client() *herdr.Client {
	return herdr.NewClient(herdr.UnixDialer(f.srv.Path), herdr.WithBackoff(10*time.Millisecond, 50*time.Millisecond))
}

// harness drives a model the way Bubble Tea would, from the test goroutine,
// with the loops running for real against the fake herdr. Messages the loops
// send are collected but not applied, so each test decides exactly what the
// model sees.
type harness struct {
	t      *testing.T
	herdr  *fakeHerdr
	clock  *clock
	model  *Model
	loops  *loops
	cancel context.CancelFunc
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	f := newFakeHerdr(t)
	clk := newClock()
	ctx, cancel := context.WithCancel(context.Background())
	client := f.client()
	m := newModel(ctx, client, clk.Now, feedPane, config.Update{Config: config.Default()})
	l := newLoops(ctx, &faults{ctx: ctx, cancel: cancel}, client, func(tea.Msg) {}, clk.Now, Timings{Poll: time.Hour, Sync: time.Hour})
	m.loops = l
	m.width, m.height = 120, 40
	h := &harness{t: t, herdr: f, clock: clk, model: m, loops: l, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		l.wait()
	})
	return h
}

func (h *harness) update(msg tea.Msg) tea.Cmd {
	h.t.Helper()
	_, cmd := h.model.Update(msg)
	return cmd
}

// press sends a key and returns the command it gave back.
func (h *harness) press(key string) tea.Cmd {
	h.t.Helper()
	var k tea.KeyMsg
	switch key {
	case "enter":
		k = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		k = tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+c":
		k = tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		k = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	return h.update(k)
}

// run runs a command and applies what it returns, as Bubble Tea would, until
// there's nothing left to run.
func (h *harness) run(cmd tea.Cmd) {
	h.t.Helper()
	for cmd != nil {
		cmd = h.update(cmd())
	}
}

// gen is the number of the loop watching pane now, which a message from that
// loop carries.
func (h *harness) gen(pane string) uint64 {
	return h.loops.panes[pane].gen
}

func (h *harness) sync() {
	h.t.Helper()
	h.update(h.loops.snapshot())
}

// catchError syncs, watches the web pane and prints an error in it.
func (h *harness) catchError() engine.Entry {
	h.t.Helper()
	h.sync()
	h.update(watchListMsg{panes: []string{webPane}})
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Initial: true, Lines: []string{"ready"}}})
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: []string{
		" GET /notices 200",
		"TypeError: Cannot read properties of undefined (reading 'map')",
		"    at NoticeList (app/notices/page.tsx:14:22)",
	}}})
	h.clock.Advance(2 * time.Second)
	h.update(tickMsg{})
	if len(h.model.entries) != 1 {
		h.t.Fatalf("got %d entries, want 1: %+v", len(h.model.entries), h.model.entries)
	}
	return h.model.entries[0]
}

func TestFirstReadIsHistory(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.update(watchListMsg{panes: []string{webPane}})
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Initial: true, Lines: []string{"TypeError: old news"}}})
	h.clock.Advance(2 * time.Second)
	h.update(tickMsg{})
	if len(h.model.entries) != 0 {
		t.Errorf("an error already on screen became an entry: %+v", h.model.entries)
	}
}

func TestEntryShowsWhereItCameFromAndWhereItGoes(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	view := h.model.View()
	for _, want := range []string{
		"Sightline · watching web",
		"● web · next dev",
		"TypeError: Cannot read properties of undefined (reading 'map')",
		"at NoticeList (app/notices/page.tsx:14:22)",
		"→ claude (tab 2)",
		"[enter] send   [d] dismiss   [n] never",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view doesn't contain %q:\n%s", want, view)
		}
	}
}

func TestEnterSendsExactlyOnce(t *testing.T) {
	h := newHarness(t)
	en := h.catchError()
	want, err := h.model.engine.Message(en.ID)
	if err != nil {
		t.Fatal(err)
	}

	cmd := h.press("enter")
	if cmd == nil {
		t.Fatalf("enter gave no command, status %q", h.model.status)
	}
	// A second press while the first is still on its way does nothing.
	if again := h.press("enter"); again != nil {
		t.Error("a second enter while sending started another send")
	}
	h.run(cmd)

	calls := h.herdr.promptCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d prompts, want 1", len(calls))
	}
	if calls[0].Target != agentPane {
		t.Errorf("sent to %q, want %q", calls[0].Target, agentPane)
	}
	if calls[0].Text != want {
		t.Errorf("sent text differs from Message:\n got %q\nwant %q", calls[0].Text, want)
	}
	if got := h.model.entries[0].State; got != engine.StateSent {
		t.Errorf("state = %v, want sent", got)
	}

	// Enter on a sent entry asks first, and sends once more only on a second press.
	if cmd := h.press("enter"); cmd != nil {
		t.Error("enter on a sent entry sent again without asking")
	}
	if !strings.Contains(h.model.status, "Press enter again") {
		t.Errorf("status = %q, want it to ask", h.model.status)
	}
	h.run(h.press("enter"))
	if got := len(h.herdr.promptCalls()); got != 2 {
		t.Errorf("got %d prompts after confirming, want 2", got)
	}
}

func TestNothingIsSentWithoutAKeypress(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	for range 5 {
		h.clock.Advance(time.Minute)
		h.update(tickMsg{})
		h.sync()
	}
	if got := len(h.herdr.promptCalls()); got != 0 {
		t.Errorf("got %d prompts without a keypress", got)
	}
}

func TestBlockedTargetIsRefused(t *testing.T) {
	tests := []struct {
		name string
		// setUp runs after the error is caught and returns the command enter gave.
		setUp func(h *harness) tea.Cmd
	}{
		{
			name: "only agent in the tab is blocked",
			setUp: func(h *harness) tea.Cmd {
				h.herdr.setStatus(agentPane, herdr.AgentBlocked)
				h.sync()
				return h.press("enter")
			},
		},
		{
			name: "picked by hand while blocked",
			setUp: func(h *harness) tea.Cmd {
				h.herdr.setStatus(agentPane, herdr.AgentBlocked)
				h.sync()
				h.press("t")
				h.press("enter")
				return h.press("enter")
			},
		},
		{
			name: "blocked between the snapshot and the keypress",
			setUp: func(h *harness) tea.Cmd {
				h.herdr.setStatus(agentPane, herdr.AgentBlocked)
				return h.press("enter")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.catchError()
			h.run(tt.setUp(h))
			if got := len(h.herdr.promptCalls()); got != 0 {
				t.Errorf("got %d prompts to a blocked agent", got)
			}
			if !strings.Contains(h.model.status, "Not sent") {
				t.Errorf("status = %q, want it to say it wasn't sent", h.model.status)
			}
			if h.model.entries[0].State != engine.StateNew {
				t.Errorf("state = %v, want new", h.model.entries[0].State)
			}
		})
	}
}

func TestStaleTargetIsRefused(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	cmd := h.press("enter")
	// The agent's pane closes after the keypress but before the check.
	h.herdr.mu.Lock()
	delete(h.herdr.status, agentPane)
	h.herdr.mu.Unlock()
	h.run(cmd)
	if got := len(h.herdr.promptCalls()); got != 0 {
		t.Errorf("got %d prompts to a closed pane", got)
	}
	if !strings.Contains(h.model.status, "closed") {
		t.Errorf("status = %q, want it to say the pane closed", h.model.status)
	}
}

func TestAskingNeverFallsBackToTheFirstCandidate(t *testing.T) {
	h := newHarness(t)
	// Two idle agents in the tab, neither seen starting work: a tie.
	h.herdr.mu.Lock()
	h.herdr.status["w1:p4"] = herdr.AgentIdle
	h.herdr.mu.Unlock()
	h.catchError()
	if cmd := h.press("enter"); cmd != nil {
		t.Fatal("enter sent while the feed was asking")
	}
	if !strings.Contains(h.model.status, "Press t to pick") {
		t.Errorf("status = %q, want it to offer t", h.model.status)
	}
	if !strings.Contains(h.model.View(), "? asking:") {
		t.Errorf("view doesn't say it's asking:\n%s", h.model.View())
	}

	// Picking one with t and then pressing enter sends to that one.
	h.press("t")
	if h.model.mode != modePick {
		t.Fatal("t didn't open the picker")
	}
	h.press("j")
	h.press("enter")
	picked := h.model.picked[h.model.entries[0].ID].PaneID
	h.run(h.press("enter"))
	calls := h.herdr.promptCalls()
	if len(calls) != 1 || calls[0].Target != picked {
		t.Errorf("prompts = %+v, want one to %s", calls, picked)
	}
}

func TestViewShowsExactlyWhatIsSent(t *testing.T) {
	h := newHarness(t)
	en := h.catchError()
	want, _ := h.model.engine.Message(en.ID)

	h.press("v")
	if h.model.mode != modeView {
		t.Fatal("v didn't open the view")
	}
	if h.model.viewText != want {
		t.Errorf("view text differs from Message:\n got %q\nwant %q", h.model.viewText, want)
	}
	h.model.width = 1000
	if got := strings.Join(h.model.viewLines(), "\n"); got != want {
		t.Errorf("drawn lines differ from Message:\n got %q\nwant %q", got, want)
	}
	// In a narrow pane long lines wrap, and nothing is lost.
	h.model.width = 30
	if got := strings.Join(h.model.viewLines(), ""); got != strings.ReplaceAll(want, "\n", "") {
		t.Errorf("wrapped lines lose text:\n got %q\nwant %q", got, want)
	}
	h.model.width, h.model.height = 120, 6
	h.press("j")
	if h.model.viewTop != 1 {
		t.Errorf("j scrolled to %d, want 1", h.model.viewTop)
	}
	h.press("G")
	if !strings.Contains(h.model.View(), "Please investigate and fix the cause.") {
		t.Errorf("the end of the message isn't reachable:\n%s", h.model.View())
	}

	// Enter from the view sends the bytes on screen.
	h.run(h.press("enter"))
	calls := h.herdr.promptCalls()
	if len(calls) != 1 || calls[0].Text != want {
		t.Errorf("prompts = %+v, want one with the viewed text", calls)
	}
	h.press("esc")
	if h.model.mode != modeList {
		t.Error("esc didn't close the view")
	}
}

func TestViewRefusesToSendAChangedMessage(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	h.press("v")
	// The same error again changes the count, and so the message.
	h.model.viewText += "something else"
	if cmd := h.press("enter"); cmd != nil {
		t.Error("enter sent a message that differs from what was on screen")
	}
	if !strings.Contains(h.model.status, "changed") {
		t.Errorf("status = %q, want it to say the message changed", h.model.status)
	}
}

func TestListKeys(t *testing.T) {
	h := newHarness(t)
	first := h.catchError()
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: []string{"panic: something else broke"}}})
	h.clock.Advance(2 * time.Second)
	h.update(tickMsg{})
	if len(h.model.entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(h.model.entries))
	}
	newest := h.model.entries[0].ID

	// A new entry doesn't move the selection from under the person.
	if h.model.selected != first.ID {
		t.Errorf("selected %s, want it to stay on %s", h.model.selected, first.ID)
	}
	h.press("j")
	if h.model.selected != first.ID {
		t.Error("j moved past the last entry")
	}
	h.press("k")
	if h.model.selected != newest {
		t.Errorf("k selected %s, want %s", h.model.selected, newest)
	}
	h.press("k")
	if h.model.selected != newest {
		t.Error("k moved past the first entry")
	}

	// With no config directory there's nowhere to save a rule (see ignore_test.go).
	h.press("n")
	if !strings.Contains(h.model.status, "no config directory") || h.model.mode != modeList {
		t.Errorf("n status = %q in mode %v, want it to say there's nowhere to save", h.model.status, h.model.mode)
	}
	if len(h.model.entries) != 2 {
		t.Error("n changed the entries")
	}

	h.press("d")
	if len(h.model.entries) != 1 || h.model.entries[0].ID != first.ID {
		t.Errorf("d left %+v, want only %s", h.model.entries, first.ID)
	}
	if h.model.selected != first.ID {
		t.Errorf("after d selected %s, want %s", h.model.selected, first.ID)
	}

	if cmd := h.press("q"); cmd == nil || cmd() != tea.Quit() {
		t.Error("q didn't quit")
	}
}

func TestAgentFinishingStartsTheFixCheck(t *testing.T) {
	tests := []struct {
		name string
		// after is what the pane prints once the agent has finished, as one
		// read, and whether that read was ambiguous.
		after     []string
		ambiguous bool
		want      engine.State
		wantView  string
	}{
		{name: "nothing ran", want: engine.StateUnverified, wantView: "? unverified"},
		{
			name:     "the request ran again and the error stayed away",
			after:    []string{" GET /notices 200", "✓ Compiled /notices in 312ms"},
			want:     engine.StateFixed,
			wantView: "✓ fixed",
		},
		{
			name:      "an ambiguous read can't be called fixed",
			after:     []string{" GET /notices 200", "✓ Compiled /notices in 312ms"},
			ambiguous: true,
			want:      engine.StateUnverified,
			wantView:  "? unverified",
		},
		{
			name:     "the error came back",
			after:    []string{"TypeError: Cannot read properties of undefined (reading 'map')", "    at NoticeList (app/notices/page.tsx:14:22)"},
			want:     engine.StateStillFailing,
			wantView: "✗ still failing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.catchError()
			h.run(h.press("enter"))

			// Seen working, then idle: finished.
			h.herdr.setStatus(agentPane, herdr.AgentWorking)
			h.sync()
			h.clock.Advance(5 * time.Second)
			h.herdr.setStatus(agentPane, herdr.AgentIdle)
			h.sync()
			if _, ok := h.model.awaiting[agentPane]; ok {
				t.Fatal("still waiting on an agent that finished")
			}

			h.clock.Advance(2 * time.Second)
			h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: tt.after, Ambiguous: tt.ambiguous}})
			h.clock.Advance(time.Duration(config.DefaultSettleSeconds)*time.Second + 2*captureWait)
			h.update(tickMsg{})

			en, ok := h.model.entry(h.model.selected)
			if !ok {
				t.Fatal("nothing selected")
			}
			if en.State != tt.want {
				t.Errorf("state = %v, want %v", en.State, tt.want)
			}
			if !strings.Contains(h.model.View(), tt.wantView) {
				t.Errorf("view doesn't show %q:\n%s", tt.wantView, h.model.View())
			}

			// Still failing sends again on one press, through the same checks.
			if tt.want == engine.StateStillFailing {
				h.run(h.press("enter"))
				if got := len(h.herdr.promptCalls()); got != 2 {
					t.Errorf("got %d prompts after resending, want 2", got)
				}
			}
		})
	}
}

func TestAgentFinishedBetweenSnapshots(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	h.run(h.press("enter"))
	// Working and idle again before any snapshot saw it: the counter moved.
	h.herdr.setStatus(agentPane, herdr.AgentWorking)
	h.herdr.setStatus(agentPane, herdr.AgentIdle)
	h.sync()
	if _, ok := h.model.awaiting[agentPane]; ok {
		t.Error("missed an agent that finished between snapshots")
	}
}

func TestAgentStillIdleIsNotFinished(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	h.run(h.press("enter"))
	h.sync()
	if _, ok := h.model.awaiting[agentPane]; !ok {
		t.Error("an agent that hasn't moved was taken as finished")
	}
}

// An agent that goes away before finishing ends the wait, however the feed
// hears of it. Events end it straight away. Snapshots take two in a row, as one
// can overtake the event saying the pane moved.
func TestAgentGoneEndsTheWait(t *testing.T) {
	tests := []struct {
		name string
		gone func(h *harness)
		// syncs is how many snapshots it takes to notice.
		syncs int
	}{
		{
			name: "its pane closed",
			gone: func(h *harness) {
				h.herdr.removeAgent(agentPane)
				h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneClosed, PaneID: agentPane}, global: true})
			},
		},
		{
			name: "it exited",
			gone: func(h *harness) {
				h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneExited, PaneID: agentPane}, global: true})
			},
		},
		{
			name: "it left the pane",
			gone: func(h *harness) {
				h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventAgentStatusChanged, PaneID: agentPane, AgentStatus: herdr.AgentUnknown}})
			},
		},
		{
			name: "another kind of agent took the pane over",
			gone: func(h *harness) {
				h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventAgentStatusChanged, PaneID: agentPane, Agent: "codex", AgentStatus: herdr.AgentIdle}})
			},
		},
		{
			name:  "snapshots stopped showing its pane",
			gone:  func(h *harness) { h.herdr.removeAgent(agentPane) },
			syncs: 2,
		},
		{
			name:  "snapshots showed another kind in its pane",
			gone:  func(h *harness) { h.herdr.setKind(agentPane, "codex") },
			syncs: 2,
		},
		{
			// The new agent being idle mustn't count as the old one finishing.
			name: "snapshots showed another kind, idle, in its pane",
			gone: func(h *harness) {
				h.herdr.setKind(agentPane, "codex")
				h.herdr.setStatus(agentPane, herdr.AgentIdle)
			},
			syncs: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.catchError()
			h.run(h.press("enter"))
			h.herdr.setStatus(agentPane, herdr.AgentWorking)
			h.sync()

			tt.gone(h)
			for i := range tt.syncs {
				if i > 0 {
					if en, _ := h.model.entry(h.model.selected); en.State != engine.StateSent {
						t.Fatalf("one snapshot ended the wait: %v", en.State)
					}
				}
				h.clock.Advance(3 * time.Second)
				h.sync()
			}
			en, _ := h.model.entry(h.model.selected)
			if en.State != engine.StateUnverified || en.Note != engine.NoteAgentGone {
				t.Fatalf("entry = %v %q, want unverified with a note", en.State, en.Note)
			}
			if _, ok := h.model.awaiting[agentPane]; ok {
				t.Error("still waiting on an agent that has gone")
			}
			if view := h.model.View(); !strings.Contains(view, engine.NoteAgentGone) {
				t.Errorf("view doesn't say why:\n%s", view)
			}

			// enter sends it again, to whichever agent is chosen now.
			const newAgent = "w1:p4"
			h.herdr.removeAgent(agentPane)
			h.herdr.setStatus(newAgent, herdr.AgentIdle)
			h.clock.Advance(time.Second) // so the snapshot is newer than any event
			h.sync()
			h.run(h.press("enter"))
			calls := h.herdr.promptCalls()
			if len(calls) != 2 || calls[1].Target != newAgent {
				t.Fatalf("prompts = %+v, want a second one to %s", calls, newAgent)
			}
			if en, _ := h.model.entry(h.model.selected); en.State != engine.StateSent || en.Note != "" {
				t.Errorf("after sending again = %v %q, want sent with no note", en.State, en.Note)
			}
		})
	}
}

// A pane that moved without the feed hearing of it is followed by its terminal
// id, not taken for an agent that went away.
func TestAgentPaneMovedUnheardIsFollowed(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	h.run(h.press("enter"))
	h.herdr.setStatus(agentPane, herdr.AgentWorking)
	h.sync()

	const moved = "w1:p7"
	h.herdr.movePane(agentPane, moved)
	for range 3 {
		h.clock.Advance(3 * time.Second)
		h.sync()
	}
	if en, _ := h.model.entry(h.model.selected); en.State != engine.StateSent {
		t.Fatalf("entry = %v %q, want still sent", en.State, en.Note)
	}
	if _, ok := h.model.awaiting[moved]; !ok {
		t.Fatal("the wait didn't follow the pane")
	}

	h.herdr.setStatus(moved, herdr.AgentIdle)
	h.clock.Advance(3 * time.Second)
	h.sync()
	if _, ok := h.model.awaiting[moved]; ok {
		t.Error("the moved agent finishing wasn't noticed")
	}
}

// Once the agent has finished, the verdict comes from the pane, so the agent
// going away during the settle period changes nothing.
func TestAgentGoneWhileSettlingKeepsTheVerdict(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	h.run(h.press("enter"))
	h.herdr.setStatus(agentPane, herdr.AgentWorking)
	h.sync()
	h.clock.Advance(5 * time.Second)
	h.herdr.setStatus(agentPane, herdr.AgentIdle)
	h.sync()

	h.herdr.removeAgent(agentPane)
	h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneClosed, PaneID: agentPane}, global: true})
	h.clock.Advance(2 * time.Second)
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: []string{" GET /notices 200", "✓ Compiled /notices in 312ms"}}})
	h.clock.Advance(time.Duration(config.DefaultSettleSeconds)*time.Second + 2*captureWait)
	h.update(tickMsg{})

	if en, _ := h.model.entry(h.model.selected); en.State != engine.StateFixed || en.Note != "" {
		t.Fatalf("entry = %v %q, want fixed", en.State, en.Note)
	}
}

func TestWatchList(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.update(watchListMsg{panes: []string{webPane, feedPane, webPane}})
	if got := h.model.watching; len(got) != 1 || got[0] != webPane {
		t.Errorf("watching %v, want only %s (never the feed itself, never twice)", got, webPane)
	}
	if _, ok := h.loops.panes[webPane]; !ok {
		t.Error("no loop for the watched pane")
	}

	h.update(paneGoneMsg{pane: webPane, gen: h.gen(webPane)})
	if len(h.model.watching) != 0 {
		t.Errorf("still watching %v after the pane closed", h.model.watching)
	}
	if _, ok := h.loops.panes[webPane]; ok {
		t.Error("the closed pane's loop is still running")
	}
	h.update(watchListMsg{panes: []string{webPane}})
	if len(h.model.watching) != 0 {
		t.Error("the watch list brought a closed pane back")
	}
}

func TestPaneMoveIsFollowed(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.update(watchListMsg{panes: []string{webPane}})
	moved := &herdr.Pane{ID: "w2:p1", WorkspaceID: "w2", TabID: "w2:t1"}
	h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneMoved, PaneID: "w2:p1", PreviousPaneID: webPane, Pane: moved}})
	if got := h.model.watching; len(got) != 1 || got[0] != "w2:p1" {
		t.Errorf("watching %v, want the moved pane", got)
	}
	// The list still names the old id, and that still means the moved pane.
	h.update(watchListMsg{panes: []string{webPane}})
	if got := h.model.watching; len(got) != 1 || got[0] != "w2:p1" {
		t.Errorf("after the list reloaded, watching %v", got)
	}
}

// An entry sent before its pane moved is judged on what the pane prints under
// its new id (#56), and the old id's loop can't reach the engine any more.
func TestPaneMoveCarriesTheEntry(t *testing.T) {
	tests := []struct {
		name string
		// settling moves the pane after the agent finished rather than before.
		settling bool
		after    []string
		want     engine.State
	}{
		{name: "sent, moved, then a recompile is fixed", after: []string{" GET /notices 200", "✓ Compiled /notices in 312ms"}, want: engine.StateFixed},
		{name: "sent, moved, then the error is still failing", after: []string{"TypeError: Cannot read properties of undefined (reading 'map')", "    at NoticeList (app/notices/page.tsx:14:22)"}, want: engine.StateStillFailing},
		// Output printed while the pane moved went unseen, so a fix can't be
		// proved. It's a gap, as it would be after a reconnect.
		{name: "moved while settling is unverified", settling: true, after: []string{" GET /notices 200", "✓ Compiled /notices in 312ms"}, want: engine.StateUnverified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			first := h.catchError()
			h.run(h.press("enter"))
			finish := func() {
				h.herdr.setStatus(agentPane, herdr.AgentWorking)
				h.sync()
				h.clock.Advance(5 * time.Second)
				h.herdr.setStatus(agentPane, herdr.AgentIdle)
				h.sync()
			}
			if tt.settling {
				finish()
			}

			const newID = "w2:p1"
			oldGen := h.gen(webPane)
			h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneMoved, PaneID: newID, PreviousPaneID: webPane}})
			if _, ok := h.loops.panes[webPane]; ok {
				t.Fatal("the old id's loop is still running")
			}
			// A read the old loop made before it stopped arrives late.
			h.update(deltaMsg{pane: webPane, gen: oldGen, delta: herdr.Delta{Lines: []string{"✓ Compiled /notices in 312ms"}}})
			h.update(deltaMsg{pane: newID, gen: h.gen(newID), delta: herdr.Delta{Initial: true, Lines: []string{"ready"}}})
			if !tt.settling {
				finish()
			}

			h.clock.Advance(2 * time.Second)
			h.update(deltaMsg{pane: newID, gen: h.gen(newID), delta: herdr.Delta{Lines: tt.after}})
			h.clock.Advance(time.Duration(config.DefaultSettleSeconds)*time.Second + 2*captureWait)
			h.update(tickMsg{})

			if len(h.model.entries) != 1 {
				t.Fatalf("got %d entries, want 1: %+v", len(h.model.entries), h.model.entries)
			}
			en := h.model.entries[0]
			if en.ID != first.ID || en.Source.ID != newID {
				t.Fatalf("entry %s from %s, want %s from %s", en.ID, en.Source.ID, first.ID, newID)
			}
			if en.State != tt.want {
				t.Errorf("state = %v, want %v", en.State, tt.want)
			}
			msg, err := h.model.engine.Message(en.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(msg, "pane "+newID+".") {
				t.Errorf("the message doesn't name the pane by its new id:\n%s", msg)
			}
		})
	}
}

// Following a move rewrites the watch list file to the new id, so unwatch on
// the moved pane finds it.
func TestPaneMoveUpdatesTheWatchList(t *testing.T) {
	dir := t.TempDir()
	if _, err := watchlist.Add(dir, webPane); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.sync()
	h.update(watchListMsg{panes: []string{webPane}})
	h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneMoved, PaneID: "w2:p1", PreviousPaneID: webPane}})
	h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneMoved, PaneID: "w3:p1", PreviousPaneID: "w2:p1"}})
	h.loops.saveMoves(dir)
	got, err := watchlist.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"w3:p1"}) {
		t.Fatalf("watch list = %q, want the pane's id after both moves", got)
	}
	h.update(watchListMsg{panes: got})
	if !slices.Equal(h.model.watching, []string{"w3:p1"}) {
		t.Fatalf("watching %v after the list was rewritten", h.model.watching)
	}
}

// A watched pane that moves while a send is in flight is sent once, waited on
// once and judged on its new id.
func TestPaneMoveDuringASend(t *testing.T) {
	h := newHarness(t)
	first := h.catchError()
	cmd := h.press("enter")
	h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneMoved, PaneID: "w2:p1", PreviousPaneID: webPane}})
	h.update(deltaMsg{pane: "w2:p1", gen: h.gen("w2:p1"), delta: herdr.Delta{Initial: true}})
	h.run(cmd)
	if got := len(h.herdr.promptCalls()); got != 1 {
		t.Fatalf("got %d prompts, want 1", got)
	}
	if len(h.model.awaiting) != 1 {
		t.Fatalf("waiting on %d agents, want 1", len(h.model.awaiting))
	}
	h.herdr.setStatus(agentPane, herdr.AgentWorking)
	h.sync()
	h.clock.Advance(5 * time.Second)
	h.herdr.setStatus(agentPane, herdr.AgentIdle)
	h.sync()
	h.clock.Advance(2 * time.Second)
	h.update(deltaMsg{pane: "w2:p1", gen: h.gen("w2:p1"), delta: herdr.Delta{Lines: []string{" GET /notices 200", "✓ Compiled /notices in 312ms"}}})
	h.clock.Advance(time.Duration(config.DefaultSettleSeconds)*time.Second + 2*captureWait)
	h.update(tickMsg{})
	if len(h.model.entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(h.model.entries))
	}
	if en := h.model.entries[0]; en.ID != first.ID || en.Source.ID != "w2:p1" || en.State != engine.StateFixed {
		t.Fatalf("entry %s from %s is %v, want %s from w2:p1 fixed", en.ID, en.Source.ID, en.State, first.ID)
	}
}

// An error caught after a move, before a snapshot has seen the new id, still
// shows the pane's label.
func TestPaneMoveKeepsTheTitle(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.update(watchListMsg{panes: []string{webPane}})
	h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneMoved, PaneID: "w2:p1", PreviousPaneID: webPane}})
	h.update(deltaMsg{pane: "w2:p1", gen: h.gen("w2:p1"), delta: herdr.Delta{Initial: true}})
	h.update(deltaMsg{pane: "w2:p1", gen: h.gen("w2:p1"), delta: herdr.Delta{Lines: []string{"TypeError: boom"}}})
	h.clock.Advance(2 * time.Second)
	h.update(tickMsg{})
	if len(h.model.entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(h.model.entries))
	}
	if got := h.model.entries[0].Source; got.ID != "w2:p1" || got.Title != "web" {
		t.Fatalf("source = %+v, want w2:p1 titled web", got)
	}
}

// Only a listed pane's move goes to the watch list file, and a write that
// fails says so.
func TestPaneMoveWrites(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.update(watchListMsg{panes: []string{webPane}})
	h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneMoved, PaneID: "w2:p5", PreviousPaneID: "w1:p7"}})
	if got := h.loops.takeMoves(); len(got) != 0 {
		t.Fatalf("an unlisted pane's move was queued: %v", got)
	}
	h.update(eventMsg{ev: herdr.Event{Kind: herdr.EventPaneMoved, PaneID: "w2:p1", PreviousPaneID: webPane}})
	if got := h.loops.takeMoves(); !slices.Equal(got, []paneMove{{webPane, "w2:p1"}}) {
		t.Fatalf("queued %v, want the watched pane's move", got)
	}
	h.update(moveSaveFailedMsg{pane: "w2:p1", err: errors.New("disk full")})
	if !strings.Contains(h.model.status, "couldn't be updated") || !strings.Contains(h.model.status, "disk full") {
		t.Fatalf("status = %q", h.model.status)
	}
}

func TestConfigChangeKeepsEntries(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	cfg := config.Default()
	cfg.Presets = []string{"go"}
	h.update(configMsg{config.Update{Config: cfg}})
	if len(h.model.entries) != 1 {
		t.Error("a config change lost the entries")
	}
	if h.model.status != "Reloaded sightline.toml." {
		t.Errorf("status = %q, want it to say the config was reloaded", h.model.status)
	}
	// The new presets apply straight away: a Node error no longer matches.
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: []string{"ReferenceError: notices is not defined"}}})
	h.clock.Advance(2 * time.Second)
	h.update(tickMsg{})
	if len(h.model.entries) != 1 {
		t.Errorf("got %d entries, want the Node error ignored under the go preset alone", len(h.model.entries))
	}
}

// TestReloadedIgnoreRuleAppliesStraightAway is what #14 needs: a new ignore rule
// dismisses a new entry it matches and leaves a sent one alone.
func TestReloadedIgnoreRuleAppliesStraightAway(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	h.run(h.press("enter"))
	if got := h.model.entries[0].State; got != engine.StateSent {
		t.Fatalf("state = %v, want sent", got)
	}
	h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: []string{"ReferenceError: notices is not defined"}}})
	h.clock.Advance(2 * time.Second)
	h.update(tickMsg{})
	if len(h.model.entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(h.model.entries))
	}

	cfg := config.Default()
	cfg.Ignores = []config.Ignore{{Regex: regexp.MustCompile(`Error`)}}
	h.update(configMsg{config.Update{Config: cfg}})

	states := map[string]engine.State{}
	for _, en := range h.model.engine.Entries() {
		states[en.Lines[0][:strings.IndexByte(en.Lines[0], ':')]] = en.State
	}
	if states["ReferenceError"] != engine.StateDismissed {
		t.Errorf("new entry is %v, want dismissed by the new rule", states["ReferenceError"])
	}
	if states["TypeError"] != engine.StateSent {
		t.Errorf("sent entry is %v, want it left alone", states["TypeError"])
	}
	if want := "Reloaded sightline.toml. Its ignore rules dismissed 1 entry."; h.model.status != want {
		t.Errorf("status = %q, want %q", h.model.status, want)
	}
}

// Adding a rule for the selected entry says so, rather than calling it dropped,
// and leaves nothing selected as a drop does.
func TestReloadedIgnoreRuleDismissesTheSelectedEntry(t *testing.T) {
	tests := []struct {
		name string
		mode string // key pressed before the reload
		want string
	}{
		{"list", "", "The selected one was among them."},
		{"view", "v", "This entry was dismissed by an ignore rule, so enter won't send it."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			en := h.catchError()
			if tt.mode != "" {
				h.press(tt.mode)
			}
			cfg := config.Default()
			cfg.Ignores = []config.Ignore{{Regex: regexp.MustCompile(`^TypeError`)}}
			h.update(configMsg{config.Update{Config: cfg}})
			if !strings.Contains(h.model.status, tt.want) {
				t.Errorf("status = %q, want it to contain %q", h.model.status, tt.want)
			}
			if h.model.selected == en.ID {
				t.Error("the dismissed entry is still selected")
			}
			if cmd := h.press("enter"); cmd != nil {
				t.Error("enter sent something after the selected entry was dismissed")
			}
		})
	}
}

func TestBrokenConfigKeepsTheLastGoodOne(t *testing.T) {
	h := newHarness(t)
	h.catchError()
	cfg := config.Default()
	cfg.Ignores = []config.Ignore{{Regex: regexp.MustCompile(`Error`)}}
	// Whatever config comes with the error (config.Watch sends the last good
	// one), nothing changes until the file is fixed.
	h.update(configMsg{config.Update{Config: cfg, Err: errors.New("line 3: bad regex")}})
	if got := h.model.entries[0].State; got != engine.StateNew {
		t.Errorf("state = %v, a broken file mustn't change anything", got)
	}
	if !strings.Contains(h.model.View(), "line 3: bad regex") {
		t.Errorf("view doesn't show the config error:\n%s", h.model.View())
	}
	if !strings.Contains(h.model.status, "last good config") {
		t.Errorf("status = %q, want it to say the last good config is in use", h.model.status)
	}
}

func TestDroppedCountIsShown(t *testing.T) {
	h := newHarness(t)
	h.sync()
	h.update(watchListMsg{panes: []string{webPane}})
	// More distinct errors than the engine keeps.
	for i := range 210 {
		h.update(deltaMsg{pane: webPane, gen: h.gen(webPane), delta: herdr.Delta{Lines: []string{"panic: failure number " + strings.Repeat("x", i%50) + string(rune('a'+i%26)) + string(rune('a'+i/26))}}})
		h.clock.Advance(2 * time.Second)
		h.update(tickMsg{})
	}
	if h.model.engine.Dropped() == 0 {
		t.Fatal("the engine didn't drop anything, so the test needs more errors")
	}
	h.model.selected = h.model.entries[len(h.model.entries)-1].ID
	if !strings.Contains(h.model.View(), "dropped to keep the feed short") {
		t.Errorf("view doesn't show the dropped count")
	}
}

func TestPaneTextCantDriveTheTerminal(t *testing.T) {
	if got := clean("web\x1b[2J\x07"); strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("clean left control characters in %q", got)
	}
}

// TestQuitStopsEverything runs the whole feed against the fake herdr, presses
// q and checks every goroutine it started has gone.
func TestQuitStopsEverything(t *testing.T) {
	f := newFakeHerdr(t)
	before := runtime.NumGoroutine()

	in, keys := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			Client:    f.client(),
			StateDir:  t.TempDir(),
			ConfigDir: t.TempDir(),
			Watch:     []string{webPane},
			Self:      feedPane,
			Every:     Timings{Poll: 20 * time.Millisecond, Sync: 20 * time.Millisecond, Tick: 20 * time.Millisecond, List: 20 * time.Millisecond},
			Program:   []tea.ProgramOption{tea.WithInput(in), tea.WithOutput(io.Discard), tea.WithoutSignalHandler()},
		})
	}()

	// Wait until the pane is being read, so every loop is running.
	deadline := time.Now().Add(5 * time.Second)
	for f.reads.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the feed never read the watched pane")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := keys.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("q didn't stop the feed")
	}
	_ = keys.Close()

	if got := len(f.promptCalls()); got != 0 {
		t.Errorf("the feed sent %d prompts with no enter pressed", got)
	}

	// Connections close a moment after their goroutines are told to stop, and
	// the fake server's own goroutines go with them.
	deadline = time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("%d goroutines before, %d after quitting:\n%s", before, runtime.NumGoroutine(), buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCancelStopsEverything is the same, but the caller cancels.
func TestCancelStopsEverything(t *testing.T) {
	f := newFakeHerdr(t)
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	in, keys := io.Pipe()
	defer keys.Close()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Client:  f.client(),
			Watch:   []string{webPane},
			Every:   Timings{Poll: 20 * time.Millisecond, Sync: 20 * time.Millisecond},
			Program: []tea.ProgramOption{tea.WithInput(in), tea.WithOutput(io.Discard), tea.WithoutSignalHandler()},
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.reads.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the feed never read the watched pane")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling didn't stop the feed")
	}
	_ = keys.Close()
	deadline = time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines before, %d after cancelling", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
