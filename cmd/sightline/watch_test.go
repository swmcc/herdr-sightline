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

// fakeHerdr is a herdr with the given panes open. plugin.pane.open opens newFeed
// in the target pane's workspace, unless openErr is set.
func fakeHerdr(t *testing.T, panes []herdr.Pane, newFeed string, openErr bool) *herdrtest.Server {
	t.Helper()
	s := herdrtest.New(t)
	s.Handle("ping", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		return map[string]any{"type": "pong", "version": "0.9.1", "protocol": 1}, nil
	})
	s.Handle("pane.list", func(herdrtest.Request) (any, *herdrtest.ErrorBody) {
		list := []map[string]any{}
		for _, p := range panes {
			list = append(list, map[string]any{"pane_id": p.ID, "workspace_id": p.WorkspaceID, "tab_id": p.WorkspaceID + ":t1"})
		}
		return map[string]any{"type": "pane_list", "panes": list}, nil
	})
	s.Handle("plugin.pane.open", func(req herdrtest.Request) (any, *herdrtest.ErrorBody) {
		if openErr {
			return nil, &herdrtest.ErrorBody{Code: "ui_busy", Message: "settings are open"}
		}
		return map[string]any{"type": "plugin_pane_opened", "plugin_pane": map[string]any{
			"plugin_id": "swmcc.sightline", "entrypoint": "feed",
			"pane": map[string]any{"pane_id": newFeed, "workspace_id": "w1"},
		}}, nil
	})
	s.Handle("plugin.pane.focus", func(req herdrtest.Request) (any, *herdrtest.ErrorBody) {
		var p struct {
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(req.Params, &p)
		return map[string]any{"type": "plugin_pane_focused", "plugin_pane": map[string]any{
			"plugin_id": "swmcc.sightline", "entrypoint": "feed",
			"pane": map[string]any{"pane_id": p.PaneID, "workspace_id": "w1"},
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

func methods(reqs []herdrtest.Request) []string {
	var out []string
	for _, r := range reqs {
		if r.Method != "ping" {
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

func noteFeeds(t *testing.T, dir string, feeds []string) {
	t.Helper()
	for _, f := range feeds {
		if err := registerFeed(dir, f); err != nil {
			t.Fatal(err)
		}
	}
}

func notedFeeds(t *testing.T, dir string) []string {
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

var someWorkspaces = []herdr.Pane{
	{ID: "w1:p1", WorkspaceID: "w1"},
	{ID: "w1:p2", WorkspaceID: "w1"},
	{ID: "w1:p9", WorkspaceID: "w1"},
	{ID: "w2:p1", WorkspaceID: "w2"},
	{ID: "w2:p5", WorkspaceID: "w2"},
}

func TestWatch(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string // added to a working environment
		list      []string          // the watch list before, nil for no file
		feeds     []string          // feed notes before
		openErr   bool
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
			wantFeeds: []string{"w1:p9"},
		},
		{
			name:      "a feed open in the workspace is focused, not doubled",
			list:      []string{"w1:p2"},
			feeds:     []string{"w1:p9"},
			wantOut:   "Watching w1:p1. The feed is already open.",
			wantCalls: []string{"pane.list", "plugin.pane.focus"},
			wantList:  []string{"w1:p2", "w1:p1"},
			wantFeeds: []string{"w1:p9"},
		},
		{
			name:      "watching twice is harmless",
			list:      []string{"w1:p1"},
			feeds:     []string{"w1:p9"},
			wantOut:   "Already watching w1:p1. The feed is already open.",
			wantCalls: []string{"pane.list", "plugin.pane.focus"},
			wantList:  []string{"w1:p1"},
			wantFeeds: []string{"w1:p9"},
		},
		{
			name:      "a feed in another workspace isn't reused",
			feeds:     []string{"w2:p5"},
			wantOut:   "Opened the feed beside it.",
			wantCalls: []string{"pane.list", "plugin.pane.open"},
			wantList:  []string{"w1:p1"},
			wantFeeds: []string{"w1:p9", "w2:p5"},
		},
		{
			name:      "a note for a feed that has gone is cleared",
			feeds:     []string{"w1:p8"},
			wantOut:   "Opened the feed beside it.",
			wantCalls: []string{"pane.list", "plugin.pane.open"},
			wantList:  []string{"w1:p1"},
			wantFeeds: []string{"w1:p9"},
		},
		{
			name:      "the feed never watches itself",
			env:       map[string]string{"HERDR_PANE_ID": "w1:p9"},
			feeds:     []string{"w1:p9"},
			wantCode:  1,
			wantErr:   "w1:p9 is the Sightline feed, which can't watch itself",
			wantCalls: []string{"pane.list"},
			wantFeeds: []string{"w1:p9"},
		},
		{
			name:      "closed panes are pruned when no feed is open",
			list:      []string{"w1:p7", "w1:p2", "w3:p1"},
			wantOut:   "Watching w1:p1.",
			wantCalls: []string{"pane.list", "plugin.pane.open"},
			wantList:  []string{"w1:p2", "w1:p1"},
			wantFeeds: []string{"w1:p9"},
		},
		{
			name:      "with a feed open old ids stay, as it follows moved panes by them",
			list:      []string{"w1:p7"},
			feeds:     []string{"w1:p9"},
			wantOut:   "The feed is already open.",
			wantCalls: []string{"pane.list", "plugin.pane.focus"},
			wantList:  []string{"w1:p7", "w1:p1"},
			wantFeeds: []string{"w1:p9"},
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
			wantFeeds: []string{"w1:p9"},
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
			s := fakeHerdr(t, someWorkspaces, "w1:p9", tt.openErr)
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
			reqs := received(s)
			if got := methods(reqs); !slices.Equal(got, tt.wantCalls) {
				t.Errorf("calls = %q, want %q", got, tt.wantCalls)
			}
			if got := readList(t, dir); !slices.Equal(got, tt.wantList) {
				t.Errorf("watch list = %q, want %q", got, tt.wantList)
			}
			if got := notedFeeds(t, dir); !slices.Equal(got, tt.wantFeeds) {
				t.Errorf("feeds = %q, want %q", got, tt.wantFeeds)
			}
		})
	}
}

// The feed opens as a split to the right of the watched pane and takes focus,
// as the README's quick start describes.
func TestWatchOpensTheFeedBesideThePane(t *testing.T) {
	s := fakeHerdr(t, someWorkspaces, "w1:p9", false)
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
		wantList  []string
		wantFeeds []string
	}{
		{name: "the last feed clears the list", self: "w1:p9", feeds: []string{"w1:p9"}},
		{name: "another feed keeps it", self: "w1:p9", feeds: []string{"w1:p9", "w2:p5"}, wantList: []string{"w1:p1"}, wantFeeds: []string{"w2:p5"}},
		{name: "a note for a feed that has gone doesn't keep it", self: "w1:p9", feeds: []string{"w1:p9", "w2:p8"}},
		{name: "a feed without a pane id still clears it", feeds: nil},
		{name: "without herdr any note keeps it", self: "w1:p9", feeds: []string{"w1:p9", "w2:p8"}, noHerdr: true, wantList: []string{"w1:p1"}, wantFeeds: []string{"w2:p8"}},
		{name: "without herdr and no other note it clears", self: "w1:p9", feeds: []string{"w1:p9"}, noHerdr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeHerdr(t, someWorkspaces, "", false)
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
			if got := readList(t, dir); !slices.Equal(got, tt.wantList) {
				t.Errorf("watch list = %q, want %q", got, tt.wantList)
			}
			if got := notedFeeds(t, dir); !slices.Equal(got, tt.wantFeeds) {
				t.Errorf("feeds = %q, want %q", got, tt.wantFeeds)
			}
		})
	}
}
