package watchlist_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/swmcc/herdr-sightline/internal/watchlist"
)

func TestRead(t *testing.T) {
	tests := []struct {
		name    string
		file    string // empty means no file at all
		want    []string
		wantErr string
	}{
		{name: "no file", want: nil},
		{name: "empty file", file: "\n", want: nil},
		{name: "two panes", file: `{"version":1,"panes":["w1:p3","w1:p4"]}`, want: []string{"w1:p3", "w1:p4"}},
		{name: "repeats dropped", file: `{"version":1,"panes":["w1:p3","w1:p3"]}`, want: []string{"w1:p3"}},
		{name: "unknown keys ignored", file: `{"version":1,"panes":["w1:p3"],"later":true}`, want: []string{"w1:p3"}},
		{name: "no version means 1", file: `{"panes":["w1:p3"]}`, want: []string{"w1:p3"}},
		{name: "newer version refused", file: `{"version":2,"panes":["w1:p3"]}`, wantErr: "newer"},
		{name: "broken json", file: `{"panes":`, wantErr: "reading watch list"},
		{
			name:    "bad id left out, the rest kept",
			file:    `{"version":1,"panes":["w1:p3","w1:p4; rm -rf /",""]}`,
			want:    []string{"w1:p3"},
			wantErr: "isn't a pane id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.file != "" {
				if err := os.WriteFile(watchlist.Path(dir), []byte(tt.file), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := watchlist.Read(dir)
			if !slices.Equal(got, tt.want) {
				t.Errorf("panes = %q, want %q", got, tt.want)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("err = %v, want none", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestAddAndRemove(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state") // doesn't exist yet
	steps := []struct {
		name    string
		do      func() (bool, error)
		changed bool
		want    []string
	}{
		{"add to nothing", func() (bool, error) { return watchlist.Add(dir, "w1:p3") }, true, []string{"w1:p3"}},
		{"add again is harmless", func() (bool, error) { return watchlist.Add(dir, "w1:p3") }, false, []string{"w1:p3"}},
		{"add another", func() (bool, error) { return watchlist.Add(dir, "w1:p4") }, true, []string{"w1:p3", "w1:p4"}},
		{"remove one", func() (bool, error) { return watchlist.Remove(dir, "w1:p3") }, true, []string{"w1:p4"}},
		{"remove one that isn't there", func() (bool, error) { return watchlist.Remove(dir, "w1:p9") }, false, []string{"w1:p4"}},
	}
	for _, s := range steps {
		changed, err := s.do()
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if changed != s.changed {
			t.Errorf("%s: changed = %v, want %v", s.name, changed, s.changed)
		}
		got, err := watchlist.Read(dir)
		if err != nil {
			t.Fatalf("%s: reading back: %v", s.name, err)
		}
		if !slices.Equal(got, s.want) {
			t.Errorf("%s: list = %q, want %q", s.name, got, s.want)
		}
	}
	if _, err := watchlist.Add(dir, "not a pane"); err == nil {
		t.Error("Add took an id with a space in it")
	}
}

func TestChangesDontWipeAListTheyCantRead(t *testing.T) {
	dir := t.TempDir()
	broken := []byte(`{"version":1,"panes":["w1:p3"`)
	if err := os.WriteFile(watchlist.Path(dir), broken, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := watchlist.Add(dir, "w1:p4"); err == nil {
		t.Error("Add rewrote a list it couldn't read")
	}
	got, _ := os.ReadFile(watchlist.Path(dir))
	if string(got) != string(broken) {
		t.Errorf("file = %q, want it untouched", got)
	}
}

func TestBadIDsAreClearedByTheNextChange(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(watchlist.Path(dir), []byte(`{"version":1,"panes":["bad id","w1:p3"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := watchlist.Add(dir, "w1:p4"); err != nil {
		t.Fatal(err)
	}
	got, err := watchlist.Read(dir)
	if err != nil {
		t.Errorf("bad id still there: %v", err)
	}
	if want := []string{"w1:p3", "w1:p4"}; !slices.Equal(got, want) {
		t.Errorf("list = %q, want %q", got, want)
	}
}
