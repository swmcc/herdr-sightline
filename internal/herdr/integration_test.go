package herdr_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// recordingReader keeps every read it passes on.
type recordingReader struct {
	reader herdr.PaneReader
	reads  []herdr.PaneRead
}

func (r *recordingReader) ReadPane(ctx context.Context, paneID string, lines int) (herdr.PaneRead, error) {
	read, err := r.reader.ReadPane(ctx, paneID, lines)
	if err == nil {
		r.reads = append(r.reads, read)
	}
	return read, err
}

// TestRealHerdr runs against the herdr this test is running inside. It only
// reads: it lists panes, reads its own pane and opens a subscription. It never
// creates, closes or types into a pane.
//
//	SIGHTLINE_HERDR_IT=1 go test -race -run TestRealHerdr -v ./internal/herdr
func TestRealHerdr(t *testing.T) {
	if os.Getenv("SIGHTLINE_HERDR_IT") != "1" {
		t.Skip("set SIGHTLINE_HERDR_IT=1 to run against a real herdr")
	}
	if os.Getenv(herdr.SocketEnv) == "" {
		t.Skipf("%s is not set, so this isn't running inside herdr", herdr.SocketEnv)
	}
	self := os.Getenv("HERDR_PANE_ID")
	if self == "" {
		t.Skip("HERDR_PANE_ID is not set, so there's no pane of our own to read")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := herdr.DialEnv(ctx)
	if err != nil {
		t.Fatalf("DialEnv: %v", err)
	}
	info, err := c.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	t.Logf("herdr %s, protocol %d", info.Version, info.Protocol)

	panes, err := c.ListPanes(ctx)
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	found := false
	for _, p := range panes {
		if p.ID == self {
			found = true
		}
	}
	if !found {
		t.Errorf("ListPanes returned %d panes, none of them %s", len(panes), self)
	}

	read, err := c.ReadPane(ctx, self, 50)
	if err != nil {
		t.Fatalf("ReadPane: %v", err)
	}
	if read.PaneID != self {
		t.Errorf("read pane %q, want %q", read.PaneID, self)
	}
	t.Logf("read %d bytes from %s, truncated %v", len(read.Text), self, read.Truncated)

	// A quiet pane polled twice must give no gap and no lines. This pane is a
	// terminal that may be redrawing, so the reads are recorded and the check
	// only applies when the pane really didn't change between them.
	quietChecked := false
	for _, p := range panes {
		rec := &recordingReader{reader: c}
		w := herdr.NewPaneWatcher(rec, p.ID)
		if _, err := w.Poll(ctx); err != nil {
			t.Fatalf("first Poll of %s: %v", p.ID, err)
		}
		d, err := w.Poll(ctx)
		if err != nil {
			t.Fatalf("second Poll of %s: %v", p.ID, err)
		}
		if rec.reads[0] != rec.reads[1] {
			continue
		}
		quietChecked = true
		t.Logf("checked quiet pane %s", p.ID)
		if d.Gap || d.Ambiguous || len(d.Lines) > 0 {
			t.Errorf("second Poll of quiet pane %s = %+v, want nothing new", p.ID, d)
		}
		break
	}
	if !quietChecked {
		t.Log("no pane stayed still between two polls, so the quiet pane check didn't run")
	}

	if _, err := c.ReadPane(ctx, "sightline-no-such-pane", 1); !herdr.IsPaneNotFound(err) {
		t.Errorf("reading a missing pane: err = %v, want pane not found", err)
	}

	subCtx, stop := context.WithCancel(ctx)
	events, err := c.Subscribe(subCtx, herdr.AgentStatusChanged(self), herdr.ScrollChanged(self), herdr.PaneClosed())
	if err != nil {
		stop()
		t.Fatalf("Subscribe: %v", err)
	}
	stop()
	for range events {
	}
}
