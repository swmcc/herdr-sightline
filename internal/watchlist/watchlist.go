// Package watchlist reads and writes the list of panes Sightline watches.
//
// The list lives in a small JSON file in the plugin's state directory
// (HERDR_PLUGIN_STATE_DIR), so the watch and unwatch actions (#5) and the feed
// (#9) never have to talk to each other directly. The actions write it and the
// feed reads it every second, starting and stopping watchers to match.
//
// The file is watching.json and looks like this:
//
//	{
//	  "version": 1,
//	  "panes": ["w1:p3", "w1:p4"]
//	}
//
// panes holds herdr pane ids in the order they were added. A missing file
// means nothing is watched. Unknown top level keys are ignored, so later
// versions can add to it. A version above 1 is refused rather than half read.
//
// Writes replace the file in one rename, so the feed never sees half of one.
// Add and Remove read, change and write, so two of them at the same moment can
// lose one change. They only run on a keypress, so that's an accepted risk.
//
// The feed only ever reads the file. A watched pane that closes drops out of
// the feed but stays in the file, and the feed remembers not to watch it again.
// herdr says closed pane ids aren't reused. A pane that moves to another
// workspace gets a new id, and the feed follows it without changing the file.
package watchlist

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// FileName is the name of the list inside the state directory.
const FileName = "watching.json"

// StateDirEnv is the environment variable herdr uses to tell a plugin where to
// keep its state.
const StateDirEnv = "HERDR_PLUGIN_STATE_DIR"

// Version is the format this package reads and writes.
const Version = 1

// MaxFileSize is the most Read will look at. A real list is a few hundred bytes.
const MaxFileSize = 64 << 10

// maxPaneIDBytes is the longest pane id accepted. herdr's look like "w1:p3".
const maxPaneIDBytes = 64

// file is the JSON on disk.
type file struct {
	Version int      `json:"version"`
	Panes   []string `json:"panes"`
}

// Path is where the list lives inside dir.
func Path(dir string) string {
	return filepath.Join(dir, FileName)
}

// ValidPaneID reports whether id looks like a herdr pane id: 1 to 64 letters,
// digits, colons, underscores, dots or hyphens.
func ValidPaneID(id string) bool {
	if id == "" || len(id) > maxPaneIDBytes {
		return false
	}
	for _, r := range id {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == ':' || r == '_' || r == '.' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// Read returns the panes in the list in dir. A missing file is an empty list,
// not an error. Ids that aren't valid are left out and reported in the error,
// with the valid ones still returned, so one bad line doesn't stop the rest
// being watched. Repeats are dropped.
func Read(dir string) ([]string, error) {
	panes, bad, err := read(dir)
	if err != nil {
		return nil, err
	}
	return panes, bad
}

// read is Read with the two kinds of failure kept apart: bad is ids that were
// left out, err is a file that couldn't be used at all.
func read(dir string) (panes []string, bad, err error) {
	path := Path(dir)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading watch list: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("reading watch list: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("reading watch list: %s isn't a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, nil, fmt.Errorf("reading watch list: %w", err)
	}
	if len(data) > MaxFileSize {
		return nil, nil, fmt.Errorf("reading watch list: %s is bigger than %d bytes", path, MaxFileSize)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil, nil
	}

	var raw file
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("reading watch list %s: %w", path, err)
	}
	if raw.Version > Version {
		return nil, nil, fmt.Errorf("reading watch list %s: version %d is newer than this Sightline understands (%d)", path, raw.Version, Version)
	}

	var errs []error
	for _, id := range raw.Panes {
		switch {
		case !ValidPaneID(id):
			errs = append(errs, fmt.Errorf("watch list %s: %q isn't a pane id", path, id))
		case !slices.Contains(panes, id):
			panes = append(panes, id)
		}
	}
	return panes, errors.Join(errs...), nil
}

// Write replaces the list in dir with panes, creating dir if it has to. It
// refuses ids that aren't valid and drops repeats.
func Write(dir string, panes []string) error {
	out := file{Version: Version, Panes: []string{}}
	for _, id := range panes {
		if !ValidPaneID(id) {
			return fmt.Errorf("writing watch list: %q isn't a pane id", id)
		}
		if !slices.Contains(out.Panes, id) {
			out.Panes = append(out.Panes, id)
		}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("writing watch list: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("writing watch list: %w", err)
	}
	tmp, err := os.CreateTemp(dir, FileName+".*")
	if err != nil {
		return fmt.Errorf("writing watch list: %w", err)
	}
	defer os.Remove(tmp.Name()) // fails harmlessly once renamed
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing watch list: %w", err)
	}
	// On disk before the rename, so a crash can't leave an empty list.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing watch list: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing watch list: %w", err)
	}
	if err := os.Rename(tmp.Name(), Path(dir)); err != nil {
		return fmt.Errorf("writing watch list: %w", err)
	}
	return nil
}

// Add puts pane on the list in dir. It reports whether the pane was new, so
// watching a pane twice is harmless. Invalid ids already in the file are
// dropped as it's rewritten.
func Add(dir, pane string) (bool, error) {
	if !ValidPaneID(pane) {
		return false, fmt.Errorf("watching %q: not a pane id", pane)
	}
	panes, err := readForChange(dir)
	if err != nil {
		return false, err
	}
	if slices.Contains(panes, pane) {
		return false, nil
	}
	return true, Write(dir, append(panes, pane))
}

// Remove takes pane off the list in dir. It reports whether it was there.
func Remove(dir, pane string) (bool, error) {
	panes, err := readForChange(dir)
	if err != nil {
		return false, err
	}
	i := slices.Index(panes, pane)
	if i < 0 {
		return false, nil
	}
	return true, Write(dir, slices.Delete(panes, i, i+1))
}

// readForChange reads the list for Add or Remove. Bad ids alone don't stop a
// change, since rewriting the file is what clears them out. A file that can't
// be read at all does, so a change can't wipe a list it didn't understand.
func readForChange(dir string) ([]string, error) {
	panes, _, err := read(dir)
	return panes, err
}
