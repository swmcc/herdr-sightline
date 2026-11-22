//go:build unix

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/config"
)

func TestFindRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	worktree := filepath.Join(base, "worktree")
	mustMkdir(t, filepath.Join(repo, ".git"), filepath.Join(repo, "app", "src"), worktree, filepath.Join(base, "plain"))
	mustWrite(t, filepath.Join(worktree, ".git"), "gitdir: /elsewhere\n")
	// A nested repo, as with a submodule, is its own.
	mustMkdir(t, filepath.Join(repo, "vendor", "lib", ".git"))
	if err := os.Symlink(filepath.Join(repo, "app"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		dir  string
		root string
		ok   bool
	}{
		{"the repo itself", repo, repo, true},
		{"deep inside it", filepath.Join(repo, "app", "src"), repo, true},
		{"a worktree's .git file", worktree, worktree, true},
		{"a nested repo", filepath.Join(repo, "vendor", "lib"), filepath.Join(repo, "vendor", "lib"), true},
		{"a symlink into the repo resolves to it", filepath.Join(base, "link"), repo, true},
		{"not in a repo", filepath.Join(base, "plain"), "", false},
		{"a directory that's gone", filepath.Join(base, "gone"), "", false},
		{"a relative path", "repo", "", false},
		{"no directory at all", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, ok, err := config.FindRoot(tt.dir)
			if err != nil {
				t.Fatal(err)
			}
			if root != tt.root || ok != tt.ok {
				t.Errorf("FindRoot = %q, %v, want %q, %v", root, ok, tt.root, tt.ok)
			}
		})
	}
}

// A repo controls its own directory, so it can put anything at
// .sightline.toml. Only a real file of a sensible size is read.
func TestReadProjectRefuses(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
		want  string
	}{
		{"a symlink out of the repo", func(t *testing.T, root string) {
			outside := filepath.Join(filepath.Dir(root), "secret.toml")
			mustWrite(t, outside, "[[ignore]]\nregex = 'x'\n")
			mustSymlink(t, outside, config.ProjectPath(root))
		}, "is a symlink"},
		{"a symlink inside the repo", func(t *testing.T, root string) {
			mustWrite(t, filepath.Join(root, "other.toml"), "")
			mustSymlink(t, filepath.Join(root, "other.toml"), config.ProjectPath(root))
		}, "is a symlink"},
		{"a fifo", func(t *testing.T, root string) {
			if err := syscall.Mkfifo(config.ProjectPath(root), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "named pipe"},
		{"a directory", func(t *testing.T, root string) { mustMkdir(t, config.ProjectPath(root)) }, "directory"},
		{"a huge file", func(t *testing.T, root string) {
			mustWrite(t, config.ProjectPath(root), "# "+strings.Repeat("x", config.MaxProjectFileSize))
		}, "bigger than 64 KiB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "repo")
			mustMkdir(t, root)
			tt.setup(t, root)
			done := make(chan error, 1)
			go func() {
				_, _, err := config.ReadProject(root)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("error %v, want one saying %q", err, tt.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ReadProject hung")
			}
		})
	}
}

func TestReadProject(t *testing.T) {
	root := t.TempDir()
	if _, exists, err := config.ReadProject(root); exists || err != nil {
		t.Fatalf("no file: exists %v, err %v", exists, err)
	}
	mustWrite(t, config.ProjectPath(root), "presets = []\n")
	data, exists, err := config.ReadProject(root)
	if !exists || err != nil || string(data) != "presets = []\n" {
		t.Fatalf("got %q, %v, %v", data, exists, err)
	}
}

func mustMkdir(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func mustWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
