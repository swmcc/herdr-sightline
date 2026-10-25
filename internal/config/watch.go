package config

import (
	"context"
	"crypto/sha256"
	"time"
)

// DefaultPollInterval is how often Watch checks the file when not told otherwise.
const DefaultPollInterval = time.Second

// Update is one result from Watch. When Err is set the file is broken and Config
// is the last good config, or the defaults if there's never been one, so the
// caller can keep going and show the error.
type Update struct {
	Config Config
	Err    error
}

// stamp is what Watch compares to decide whether the file has changed. It hashes
// the contents rather than trusting the mod time, so an edit that keeps the same
// size within one mod time tick is still seen. The file is small and capped at
// MaxFileSize, so reading it every poll is cheap.
type stamp struct {
	exists bool
	sum    [sha256.Size]byte
	// errMsg rather than an error, so two stamps with the same failure compare equal.
	errMsg string
}

// Watch loads the config file in dir straight away and again whenever it changes,
// checking every interval. Deleting the file goes back to the defaults. Once ctx
// is cancelled the goroutine stops and the channel is closed. An interval of zero
// or less means DefaultPollInterval.
//
// A change is only used once the file has read the same for two checks in a row,
// so a save that's still being written isn't caught half way. That costs one
// interval of delay.
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

		path := Path(dir)
		good := Default()
		var last, pending stamp
		first := true
		for {
			data, exists, readErr := readFile(path)
			current := stamp{exists: exists, sum: sha256.Sum256(data)}
			if readErr != nil {
				current = stamp{errMsg: readErr.Error()}
			}
			// Wait for the file to settle before using it, except on the first
			// check, which should answer straight away.
			changed := current != last && current == pending
			pending = current
			if first || changed {
				first = false
				last = current
				u := Update{Err: readErr}
				switch {
				case readErr != nil:
				case !exists:
					u.Config = Default()
				default:
					u.Config, u.Err = Parse(path, data)
				}
				if u.Err != nil {
					u.Config = good
				} else {
					good = u.Config
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
