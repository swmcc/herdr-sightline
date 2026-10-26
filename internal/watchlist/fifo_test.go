//go:build unix

package watchlist_test

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/watchlist"
)

// A FIFO at the list's path must be refused straight away, not block the
// feed's list loop waiting for a writer.
func TestReadRefusesAFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, watchlist.FileName), 0o600); err != nil {
		t.Skipf("can't make a FIFO here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := watchlist.Read(dir)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Errorf("err = %v, want it to say it isn't a regular file", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read blocked on a FIFO")
	}
}
