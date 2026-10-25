package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// Update is one result from Watch. Exactly one of Config and Err is meaningful.
// When Err is set the file is broken, and the caller should keep using the last
// good Config and show the error.
type Update struct {
	Config Config
	Err    error
}

// DefaultPollInterval is how often Watch checks the file when not told otherwise.
const DefaultPollInterval = time.Second

// stamp is what Watch compares to decide whether the file has changed. Polling a
// stat is cheap at this size and saves pulling in a file notification library.
type stamp struct {
	exists  bool
	size    int64
	modTime time.Time
	// errMsg rather than an error, so two stamps with the same failure compare equal.
	errMsg string
}

// Watch loads the config file in dir straight away and again whenever it changes,
// checking every interval. Deleting the file goes back to the defaults. Once ctx
// is cancelled the goroutine stops and the channel is closed. An interval of zero
// or less means DefaultPollInterval.
//
// Changes are spotted by size and mod time. A change is only read once the file
// has looked the same for two checks in a row, so a save that's still being
// written isn't caught half way. That costs one interval of delay.
//
// A broken file comes back as an Update with Err set. Watch never stops because
// of a bad file, so fixing it and saving again is enough.
func Watch(ctx context.Context, dir string, interval time.Duration) <-chan Update {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	updates := make(chan Update)
	go func() {
		defer close(updates)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		var last, pending stamp
		first := true
		for {
			current, statErr := statFile(Path(dir))
			// Wait for the file to settle before reading it, except on the first
			// check, which should answer straight away.
			changed := current != last && current == pending
			pending = current
			if first || changed {
				first = false
				last = current
				var u Update
				if statErr != nil {
					u.Err = statErr
				} else {
					u.Config, u.Err = Load(dir)
				}
				if ctx.Err() != nil {
					return
				}
				select {
				case updates <- u:
				case <-ctx.Done():
					return
				}
			}

			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return updates
}

// statFile records a stat error in the stamp as well as returning it, so the same
// failure is reported once rather than on every tick.
func statFile(path string) (stamp, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return stamp{}, nil
	}
	if err != nil {
		return stamp{errMsg: err.Error()}, fmt.Errorf("checking config: %w", err)
	}
	return stamp{exists: true, size: info.Size(), modTime: info.ModTime()}, nil
}
