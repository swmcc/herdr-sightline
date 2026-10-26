//go:build unix

package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO blocks whoever opens it for reading until something writes to it. If
// Load did that, Watch would hang where it can't see its context.
func TestLoadRefusesSpecialFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{"fifo", func(t *testing.T, dir string) { mkfifo(t, Path(dir)) }, "named pipe"},
		{"symlink to a fifo", func(t *testing.T, dir string) {
			target := filepath.Join(dir, "pipe")
			mkfifo(t, target)
			if err := os.Symlink(target, Path(dir)); err != nil {
				t.Fatalf("symlink: %v", err)
			}
		}, "named pipe"},
		{"directory", func(t *testing.T, dir string) {
			if err := os.Mkdir(Path(dir), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}, "directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)

			done := make(chan error, 1)
			go func() {
				_, err := Load(dir)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "not a regular file") || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("Load error = %v, want it to say it's not a regular file (%s)", err, tt.want)
				}
			case <-time.After(2 * time.Second):
				// Open the pipe for writing so the stuck Load can finish and the
				// test doesn't leak a goroutine.
				if f, err := os.OpenFile(Path(dir), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					f.Close()
				}
				t.Fatal("Load blocked")
			}
		})
	}
}

func TestWatchStopsWithAFifo(t *testing.T) {
	dir := t.TempDir()
	mkfifo(t, Path(dir))
	ctx, cancel := context.WithCancel(context.Background())
	updates := Watch(ctx, dir, pollInterval)

	u := next(t, updates, "initial load")
	if u.Err == nil {
		t.Fatal("got no error for a FIFO")
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, open := <-updates:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed after cancel")
		}
	}
}

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("can't make a FIFO here: %v", err)
	}
}
