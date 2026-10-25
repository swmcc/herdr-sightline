package config

import (
	"context"
	"os"
	"testing"
	"time"
)

const pollInterval = 5 * time.Millisecond

func TestWatchFollowsTheFile(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := Watch(ctx, dir, pollInterval)

	// Watch hashes the contents, so a change is seen even when the size and mod
	// time stay the same. "settle_seconds = 7" to "settle_seconds = 8" checks that.
	// On an error the update carries the last good config.
	steps := []struct {
		name    string
		change  func()
		wantErr bool
		settle  int
	}{
		{"no file gives the defaults", func() {}, false, DefaultSettleSeconds},
		{"a new file is read", func() { writeConfig(t, dir, "settle_seconds = 5\n") }, false, 5},
		{"an edit is picked up", func() { writeConfig(t, dir, "settle_seconds = 120\n") }, false, 120},
		{"a broken file is an error, not a crash", func() { writeConfig(t, dir, "settle_seconds = = 10\n") }, true, 120},
		{"fixing it recovers", func() { writeConfig(t, dir, "settle_seconds = 7\n") }, false, 7},
		{"a same size edit is picked up", func() { writeConfig(t, dir, "settle_seconds = 8\n") }, false, 8},
		{"deleting it goes back to the defaults", func() {
			if err := os.Remove(Path(dir)); err != nil {
				t.Fatalf("removing config: %v", err)
			}
		}, false, DefaultSettleSeconds},
	}
	for _, step := range steps {
		step.change()
		u := next(t, updates, step.name)
		if step.wantErr {
			if u.Err == nil {
				t.Fatalf("%s: got no error", step.name)
			}
		} else if u.Err != nil {
			t.Fatalf("%s: %v", step.name, u.Err)
		}
		if u.Config.SettleSeconds != step.settle {
			t.Fatalf("%s: SettleSeconds = %d, want %d", step.name, u.Config.SettleSeconds, step.settle)
		}
	}
}

func TestWatchBrokenFirstLoadGivesDefaults(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "settle_seconds = = 1\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	u := next(t, Watch(ctx, dir, pollInterval), "initial load")
	if u.Err == nil {
		t.Fatal("got no error for a broken file")
	}
	if u.Config.SettleSeconds != DefaultSettleSeconds || len(u.Config.Presets) == 0 {
		t.Fatalf("Config = %+v, want the defaults", u.Config)
	}
}

func TestWatchIsQuietWhenNothingChanges(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "settle_seconds = 5\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := Watch(ctx, dir, pollInterval)
	next(t, updates, "initial load")

	select {
	case u := <-updates:
		t.Fatalf("got an update with no change: %+v", u)
	case <-time.After(20 * pollInterval):
	}
}

func TestWatchStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	updates := Watch(ctx, t.TempDir(), pollInterval)
	next(t, updates, "initial load")
	cancel()

	deadline := time.After(time.Second)
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

func TestWatchStopsWhenCancelledMidSend(t *testing.T) {
	// Nobody reads the initial load, so the goroutine is blocked sending when the
	// context is cancelled. It must still give up and close the channel.
	ctx, cancel := context.WithCancel(context.Background())
	updates := Watch(ctx, t.TempDir(), pollInterval)
	time.Sleep(10 * pollInterval)
	cancel()

	select {
	case <-updates:
		// Either the pending update or the close. Drain until closed.
		for range updates {
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestWatchWithNoIntervalUsesTheDefault(t *testing.T) {
	// time.NewTicker panics on zero, which would take the feed down with it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	next(t, Watch(ctx, t.TempDir(), 0), "initial load")
}

func next(t *testing.T, updates <-chan Update, step string) Update {
	t.Helper()
	select {
	case u, open := <-updates:
		if !open {
			t.Fatalf("%s: channel closed early", step)
		}
		return u
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: no update", step)
	}
	return Update{}
}
