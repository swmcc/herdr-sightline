package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// TrustFileName is where trusted project files are recorded, inside the
// plugin's state directory (HERDR_PLUGIN_STATE_DIR). A repo can't write there.
const TrustFileName = "trusted-projects.json"

// trustVersion is the layout of the trust file. A file with another version
// was written by another Sightline and isn't read, so nothing is trusted by
// mistake.
const trustVersion = 1

// Sum is the sha256 of a project file's exact bytes. Trust is given to one
// version of a file, so any change at all means asking again.
type Sum [sha256.Size]byte

// SumOf hashes a project file's contents.
func SumOf(data []byte) Sum { return sha256.Sum256(data) }

// String is the sum in hex, as the trust file keeps it.
func (s Sum) String() string { return hex.EncodeToString(s[:]) }

// Trust is the project files the person has trusted: one version of the file
// for each repo, by the repo's real path.
type Trust struct {
	byRoot map[string]string
}

// Trusted reports whether the project file with sum at root has been trusted.
func (t Trust) Trusted(root string, sum Sum) bool {
	got, ok := t.byRoot[root]
	return ok && got == sum.String()
}

// Roots are the repos with a trusted file, sorted.
func (t Trust) Roots() []string {
	roots := make([]string, 0, len(t.byRoot))
	for r := range t.byRoot {
		roots = append(roots, r)
	}
	slices.Sort(roots)
	return roots
}

type trustFile struct {
	Version  int            `json:"version"`
	Projects []trustedEntry `json:"projects"`
}

type trustedEntry struct {
	Root   string `json:"root"`
	SHA256 string `json:"sha256"`
}

// TrustPath is the trust file inside dir.
func TrustPath(dir string) string { return filepath.Join(dir, TrustFileName) }

// LoadTrust reads the trust file from dir. A missing file means nothing is
// trusted. A file that can't be read or understood is an error, and the caller
// should treat it as nothing trusted.
func LoadTrust(dir string) (Trust, error) {
	if dir == "" {
		return Trust{}, nil
	}
	path := TrustPath(dir)
	data, exists, err := readFile(path)
	if err != nil {
		return Trust{}, fmt.Errorf("reading trusted projects: %w", err)
	}
	return parseTrust(path, data, exists)
}

func parseTrust(path string, data []byte, exists bool) (Trust, error) {
	if !exists {
		return Trust{}, nil
	}
	var f trustFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Trust{}, fmt.Errorf("reading trusted projects from %s: %w", path, err)
	}
	if f.Version != trustVersion {
		return Trust{}, fmt.Errorf("reading trusted projects from %s: version %d isn't one this Sightline knows", path, f.Version)
	}
	t := Trust{byRoot: map[string]string{}}
	for _, p := range f.Projects {
		sum, err := hex.DecodeString(p.SHA256)
		if !filepath.IsAbs(p.Root) || err != nil || len(sum) != sha256.Size {
			return Trust{}, fmt.Errorf("reading trusted projects from %s: %q isn't a repo path and a sha256", path, p.Root)
		}
		t.byRoot[p.Root] = strings.ToLower(p.SHA256)
	}
	return t, nil
}

// TrustProject records that the project file with sum at root is trusted,
// replacing any version trusted before.
func TrustProject(dir, root string, sum Sum) error {
	return changeTrust(dir, func(t Trust) { t.byRoot[root] = sum.String() })
}

// ForgetProject stops trusting root's project file, whatever its version.
func ForgetProject(dir, root string) error {
	return changeTrust(dir, func(t Trust) { delete(t.byRoot, root) })
}

// errTrustRace says the trust file changed between reading it and replacing it.
var errTrustRace = errors.New("trusted projects changed while Sightline was writing them, so nothing was recorded. Try again")

// changeTrust reads the trust file, changes it and writes it back in one
// rename. A file that can't be read is left alone rather than overwritten, so
// a mistake in it is seen rather than lost.
//
// Two feeds can be open at once, and each one writes the whole file, so the
// read and the write happen under a lock beside the file. Otherwise one feed's
// x could be undone by the other's y. The file is also read again just before
// the rename, in case something that doesn't take the lock changed it.
func changeTrust(dir string, change func(Trust)) error {
	if dir == "" {
		return fmt.Errorf("there's no state directory to record trust in (HERDR_PLUGIN_STATE_DIR isn't set)")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("making the state directory: %w", err)
	}
	unlock, err := lockFile(TrustPath(dir) + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	for range 3 {
		err := changeTrustOnce(dir, change)
		if !errors.Is(err, errTrustRace) {
			return err
		}
	}
	return errTrustRace
}

func changeTrustOnce(dir string, change func(Trust)) error {
	path := TrustPath(dir)
	raw, exists, err := readFile(path)
	if err != nil {
		return fmt.Errorf("reading trusted projects: %w", err)
	}
	t, err := parseTrust(path, raw, exists)
	if err != nil {
		return err
	}
	if t.byRoot == nil {
		t.byRoot = map[string]string{}
	}
	change(t)
	f := trustFile{Version: trustVersion, Projects: []trustedEntry{}}
	for _, root := range t.Roots() {
		f.Projects = append(f.Projects, trustedEntry{Root: root, SHA256: t.byRoot[root]})
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("writing trusted projects: %w", err)
	}
	unchanged := func() bool {
		now, nowExists, err := readFile(path)
		return err == nil && nowExists == exists && bytes.Equal(now, raw)
	}
	return writeAtomic(path, append(data, '\n'), unchanged)
}

// writeAtomic replaces path with data through a temporary file and a rename,
// so a crash part way leaves the old file. unchanged is asked just before the
// rename, and if it says the file has moved on, nothing is written and the
// error is errTrustRace. A write landing between that check and the rename is
// still lost, but the gap is tiny.
func writeAtomic(path string, data []byte, unchanged func() bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	if !unchanged() {
		return errTrustRace
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	done = true
	syncDir(filepath.Dir(path))
	return nil
}
