package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/swmcc/herdr-sightline/internal/herdr"
	"github.com/swmcc/herdr-sightline/internal/herdr/herdrtest"
	"github.com/swmcc/herdr-sightline/internal/watchlist"
)

// term is the terminal id the fake gives a pane that started out as pane.
func term(pane string) string { return "term_" + strings.ReplaceAll(pane, ":", "_") }

// pane is a fake pane that has never moved.
func pane(id string) herdr.Pane {
	ws, _, _ := strings.Cut(id, ":")
	return herdr.Pane{ID: id, TerminalID: term(id), WorkspaceID: ws}
}

// fakeOptions says how the fake herdr behaves.
type fakeOptions struct {
	panes     []herdr.Pane
	newFeed   string // the pane plugin.pane.open opens
	openErr   bool
	focusFail bool
}

func fakeHerdr(t *testing.T, o fakeOptions) *herdrtest.Server {
	t.Helper()
	s := herdrtest.New(t)
	s.Handle("ping", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		return map[string]any{"type": "pong", "version": "0.9.1", "protocol": 1}, nil
	})
	s.Handle("pane.list", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		list := []map[string]any{}
		for _, p := range o.panes {
			list = append(list, map[string]any{"pane_id": p.ID, "terminal_id": p.TerminalID, "workspace_id": p.WorkspaceID})
		}
		return map[string]any{"type": "pane_list", "panes": list}, nil
	})
	s.Handle("plugin.pane.open", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		if o.openErr {
			return nil, &herdrtest.ErrorBody{Code: "ui_busy", Message: "settings are open"}
		}
		p := pane(o.newFeed)
		return map[string]any{"type": "plugin_pane_opened", "plugin_pane": map[string]any{
			"plugin_id": "swmcc.sightline", "entrypoint": "feed",
			"pane": map[string]any{"pane_id": p.ID, "terminal_id": p.TerminalID, "workspace_id": p.WorkspaceID},
		}}, nil
	})
	s.Handle("plugin.pane.focus", func(req herdrtest.Request) (any, *herdrtest.ErrorBody) {
		if o.focusFail {
			return nil, &herdrtest.ErrorBody{Code: "pane_not_found", Message: "no such pane"}
		}
		var p struct {
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(req.Params, &p)
		return map[string]any{"type": "plugin_pane_focused", "plugin_pane": map[string]any{
			"plugin_id": "swmcc.sightline", "entrypoint": "feed",
			"pane": map[string]any{"pane_id": p.PaneID},
		}}, nil
	})
	return s
}

// received drains the requests the server has had, after the command finished.
func received(s *herdrtest.Server) []herdrtest.Request {
	var out []herdrtest.Request
	for {
		select {
		case req := <-s.Requests():
			out = append(out, req)
		default:
			return out
		}
	}
}

// calls is what was asked of herdr, leaving out pings, with the pane for
// focus calls.
func calls(reqs []herdrtest.Request) []string {
	var out []string
	for _, r := range reqs {
		switch r.Method {
		case "ping":
		case "plugin.pane.focus":
			var p struct {
				PaneID string `json:"pane_id"`
			}
			_ = json.Unmarshal(r.Params, &p)
			out = append(out, r.Method+" "+p.PaneID)
		default:
			out = append(out, r.Method)
		}
	}
	return out
}

func writeList(t *testing.T, dir string, panes []string) {
	t.Helper()
	if panes == nil {
		return
	}
	if err := watchlist.Write(dir, panes); err != nil {
		t.Fatal(err)
	}
}

func noteFeeds(t *testing.T, dir string, terminals []string) {
	t.Helper()
	for _, f := range terminals {
		if err := registerFeed(dir, f); err != nil {
			t.Fatal(err)
		}
	}
}

func feedNotes(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, feedsDir))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func readList(t *testing.T, dir string) []string {
	t.Helper()
	got, err := watchlist.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

var someWorkspaces = []herdr.Pane{pane("w1:p1"), pane("w1:p2"), pane("w1:p9"), pane("w2:p1"), pane("w2:p5")}

func TestWatch(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string // added to a working environment
		panes     []herdr.Pane      // nil for someWorkspaces
		list      []string          // the watch list before, nil for no file
		feeds     []string          // feed notes before, as terminal ids
		openErr   bool
		focusFail bool
		wantCode  int
		wantOut   string
		wantErr   string
		wantCalls []string
		wantList  []string
		wantFeeds []string
	}{
		{
			name:      "first watch opens the feed beside the pane",
			wantOut:   "Watching w1:p1. Opened the feed beside it.",
			wantCalls: []string{"pane.list", "plugin.pane.open"},
			wantList:  []string{"w1:p1"},
			wantFeeds: []string{term("w1:p9")},
		},
		{
			name:      "an open feed is focused, not doubled",
			list:      []string{"w1:p2"},
			feeds:     []string{term("w1:p9")},
			wantOut:   "Watching w1:p1. The feed is already open in w1:p9.",
			wantCalls: []string{"pane.list", "plugin.pane.focus w1:p9"},
			wantList:  []string{"w1:p2", "w1:p1"},
			wantFeeds: []string{term("w1:p9")},
		},
		{
			name:      "watching twice is harmless",
			list:      []string{"w1:p1"},
			feeds:     []string{term("w1:p9")},
			wantOut:   "Already watching w1:p1. The feed is already open in w1:p9.",
			wantCalls: []string{"pane.list", "plugin.pane.focus w1:p9"},
			wantList:  []string{"w1:p1"},
			wantFeeds: []string{term("w1:p9")},
		},
		{
			name:      "a feed in another workspace is reused, as every feed watches the whole list",
			feeds:     []string{term("w2:p5")},
			wantOut:   "The feed is already open in w2:p5.",
			wantCalls: []string{"pane.list", "plugin.pane.focus w2:p5"},
			wantList:  []string{"w1:p1"},
			wantFeeds: []string{term("w2:p5")},
		},
		{
			name:      "the feed in the pane's own workspace comes first",
			feeds:     []string{term("w2:p5"), term("w1:p9")},
			wantOut:   "The feed is already open in w1:p9.",
			wantCalls: []string{"pane.list", "plugin.pane.focus w1:p9"},
			wantList:  []string{"w1:p1"},
			wantFeeds: []string{term("w1:p9"), term("w2:p5")},
		},
		{
			name:      "a feed that can't be focused is passed over",
			feeds:     []string{term("w2:p5")},
			focusFail: true,
			wantOut:   "Opened the feed beside it.",
			wantCalls: []string{"pane.list", "plugin.pane.focus w2:p5", "plugin.pane.open"},
			wantList:  []string{"w1:p1"},
			wantFeeds: []string{term("w1:p9"), term("w2:p5")},
		},
		{
			name:      "a feed that moved is still found by its terminal",
			panes:     []herdr.Pane{pane("w1:p1"), {ID: "w2:p7", TerminalID: term("w1:p9"), WorkspaceID: "w2"}},
			list:      []string{"w1:p4"},
			feeds:     []string{term("w1:p9")},
			wantOut:   "The feed is already open in w2:p7.",
			wantCalls: []string{"pane.list", "plugin.pane.focus w2:p7"},
			wantList:  []string{"w1:p4", "w1:p1"},
			wantFeeds: []string{term("w1:p9")},
		},
		{
			name:      "a note for a feed that has gone is cleared",
			feeds:     []string{"term_gone"},
			wantOut:   "Opened the feed beside it.",
			wantCalls: []string{"pane.list", "plugin.pane.open"},
			wantList:  []string{"w1:p1"},
			wantFeeds: []string{term("w1:p9")},
		},
		{
			name:      "the feed never watches itself",
			env:       map[string]string{"HERDR_PANE_ID": "w1:p9"},
			feeds:     []string{term("w1:p9")},
			wantCode:  1,
			wantErr:   "w1:p9 is the Sightline feed, which can't watch itself",
			wantCalls: []string{"pane.list"},
			wantFeeds: []string{term("w1:p9")},
		},
		{
			name:      "closed panes are pruned when no feed is open",
			list:      []string{"w1:p7", "w1:p2", "w3:p1"},
			wantOut:   "Watching w1:p1.",
			wantCalls: []string{"pane.list", "plugin.pane.open"},
			wantList:  []string{"w1:p2", "w1:p1"},
			wantFeeds: []string{term("w1:p9")},
		},
		{
			name:      "with a feed open old ids stay, as it follows moved panes by them",
			list:      []string{"w1:p7"},
			feeds:     []string{term("w1:p9")},
			wantOut:   "The feed is already open in w1:p9.",
			wantCalls: []string{"pane.list", "plugin.pane.focus w1:p9"},
			wantList:  []string{"w1:p7", "w1:p1"},
			wantFeeds: []string{term("w1:p9")},
		},
		{
			name:      "a pane that isn't open is refused",
			env:       map[string]string{"HERDR_PANE_ID": "w4:p1"},
			wantCode:  1,
			wantErr:   "pane w4:p1 isn't open",
			wantCalls: []string{"pane.list"},
		},
		{
			name:      "the feed failing to open is reported, with the pane still listed",
			openErr:   true,
			wantCode:  1,
			wantErr:   "w1:p1 is on the watch list, but the feed couldn't be opened: herdr plugin.pane.open: settings are open (ui_busy)",
			wantCalls: []string{"pane.list", "plugin.pane.open"},
			wantList:  []string{"w1:p1"},
		},
		{
			name:      "the pane can come from the context json",
			env:       map[string]string{"HERDR_PANE_ID": "", "HERDR_PLUGIN_CONTEXT_JSON": `{"focused_pane_id":"w1:p2","workspace_id":"w1"}`},
			wantOut:   "Watching w1:p2.",
			wantCalls: []string{"pane.list", "plugin.pane.open"},
			wantList:  []string{"w1:p2"},
			wantFeeds: []string{term("w1:p9")},
		},
		{
			name:     "no focused pane",
			env:      map[string]string{"HERDR_PANE_ID": ""},
			wantCode: 1,
			wantErr:  "herdr didn't say which pane is focused",
		},
		{
			name:     "a pane id that isn't one",
			env:      map[string]string{"HERDR_PANE_ID": "w1:p1; rm -rf /"},
			wantCode: 1,
			wantErr:  "isn't a pane id",
		},
		{
			name:     "no state dir",
			env:      map[string]string{watchlist.StateDirEnv: ""},
			wantCode: 1,
			wantErr:  "HERDR_PLUGIN_STATE_DIR is not set",
		},
		{
			name:     "no herdr",
			env:      map[string]string{herdr.SocketEnv: ""},
			wantCode: 1,
			wantErr:  "HERDR_SOCKET_PATH is not set",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			panes := tt.panes
			if panes == nil {
				panes = someWorkspaces
			}
			s := fakeHerdr(t, fakeOptions{panes: panes, newFeed: "w1:p9", openErr: tt.openErr, focusFail: tt.focusFail})
			dir := t.TempDir()
			writeList(t, dir, tt.list)
			noteFeeds(t, dir, tt.feeds)
			env := map[string]string{
				"HERDR_PANE_ID":       "w1:p1",
				herdr.SocketEnv:       s.Path,
				watchlist.StateDirEnv: dir,
			}
			for k, v := range tt.env {
				env[k] = v
			}

			var stdout, stderr bytes.Buffer
			code := run([]string{"watch"}, func(k string) string { return env[k] }, strings.NewReader(""), &stdout, &stderr)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantOut)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
			if got := calls(received(s)); !slices.Equal(got, tt.wantCalls) {
				t.Errorf("calls = %q, want %q", got, tt.wantCalls)
			}
			if got := readList(t, dir); !slices.Equal(got, tt.wantList) {
				t.Errorf("watch list = %q, want %q", got, tt.wantList)
			}
			if got := feedNotes(t, dir); !slices.Equal(got, tt.wantFeeds) {
				t.Errorf("feeds = %q, want %q", got, tt.wantFeeds)
			}
		})
	}
}

// The feed opens as a split to the right of the watched pane and takes focus,
// as the README's quick start describes.
func TestWatchOpensTheFeedBesideThePane(t *testing.T) {
	s := fakeHerdr(t, fakeOptions{panes: someWorkspaces, newFeed: "w1:p9"})
	env := map[string]string{
		"HERDR_PANE_ID":       "w1:p2",
		"HERDR_PLUGIN_ID":     "swmcc.sightline",
		herdr.SocketEnv:       s.Path,
		watchlist.StateDirEnv: t.TempDir(),
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"watch"}, func(k string) string { return env[k] }, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
	}
	var open *herdrtest.Request
	for _, r := range received(s) {
		if r.Method == "plugin.pane.open" {
			open = &r
		}
	}
	if open == nil {
		t.Fatal("plugin.pane.open wasn't called")
	}
	var got map[string]any
	if err := json.Unmarshal(open.Params, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"plugin_id":      "swmcc.sightline",
		"entrypoint":     "feed",
		"placement":      "split",
		"target_pane_id": "w1:p2",
		"direction":      "right",
		"focus":          true,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestUnwatch(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		list     []string
		wantCode int
		wantOut  string
		wantErr  string
		wantList []string
	}{
		{name: "removes the pane", list: []string{"w1:p1", "w1:p2"}, wantOut: "Stopped watching w1:p1.", wantList: []string{"w1:p2"}},
		{name: "a pane that wasn't watched", list: []string{"w1:p2"}, wantOut: "w1:p1 wasn't being watched.", wantList: []string{"w1:p2"}},
		{name: "no list at all", wantOut: "w1:p1 wasn't being watched."},
		{name: "no focused pane", env: map[string]string{"HERDR_PANE_ID": ""}, wantCode: 1, wantErr: "which pane is focused"},
		{name: "no state dir", env: map[string]string{watchlist.StateDirEnv: ""}, wantCode: 1, wantErr: "HERDR_PLUGIN_STATE_DIR is not set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeList(t, dir, tt.list)
			env := map[string]string{"HERDR_PANE_ID": "w1:p1", watchlist.StateDirEnv: dir}
			for k, v := range tt.env {
				env[k] = v
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"unwatch"}, func(k string) string { return env[k] }, strings.NewReader(""), &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantOut)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
			if got := readList(t, dir); !slices.Equal(got, tt.wantList) {
				t.Errorf("watch list = %q, want %q", got, tt.wantList)
			}
		})
	}
}

func TestEndFeed(t *testing.T) {
	tests := []struct {
		name      string
		self      string
		feeds     []string
		noHerdr   bool
		wantCalls []string
		wantList  []string
		wantFeeds []string
	}{
		{name: "the last feed clears the list without asking herdr", self: term("w1:p9"), feeds: []string{term("w1:p9")}},
		{
			name: "another feed keeps it", self: term("w1:p9"), feeds: []string{term("w1:p9"), term("w2:p5")},
			wantCalls: []string{"pane.list"}, wantList: []string{"w1:p1"}, wantFeeds: []string{term("w2:p5")},
		},
		{
			name: "a note for a feed that has gone doesn't keep it", self: term("w1:p9"), feeds: []string{term("w1:p9"), "term_gone"},
			wantCalls: []string{"pane.list"},
		},
		{name: "a feed that couldn't note itself still clears it"},
		{
			name: "without herdr any note keeps it", self: term("w1:p9"), feeds: []string{term("w1:p9"), "term_gone"}, noHerdr: true,
			wantList: []string{"w1:p1"}, wantFeeds: []string{"term_gone"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeHerdr(t, fakeOptions{panes: someWorkspaces})
			if tt.noHerdr {
				s.Close()
			}
			dir := t.TempDir()
			writeList(t, dir, []string{"w1:p1"})
			noteFeeds(t, dir, tt.feeds)
			client := herdr.NewClient(herdr.UnixDialer(s.Path))

			if err := endFeed(client, dir, tt.self); err != nil {
				t.Fatalf("endFeed: %v", err)
			}
			if got := calls(received(s)); !slices.Equal(got, tt.wantCalls) {
				t.Errorf("calls = %q, want %q", got, tt.wantCalls)
			}
			if got := readList(t, dir); !slices.Equal(got, tt.wantList) {
				t.Errorf("watch list = %q, want %q", got, tt.wantList)
			}
			if got := feedNotes(t, dir); !slices.Equal(got, tt.wantFeeds) {
				t.Errorf("feeds = %q, want %q", got, tt.wantFeeds)
			}
		})
	}
}

func TestSelfTerminal(t *testing.T) {
	s := fakeHerdr(t, fakeOptions{panes: someWorkspaces})
	client := herdr.NewClient(herdr.UnixDialer(s.Path))
	tests := []struct{ self, want string }{
		{"w1:p9", term("w1:p9")},
		{"w9:p9", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := selfTerminal(t.Context(), client, tt.self); got != tt.want {
			t.Errorf("selfTerminal(%q) = %q, want %q", tt.self, got, tt.want)
		}
	}
}
