package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Appended is an [[ignore]] table AppendIgnore added, so RemoveAppended can take
// exactly that table back out again.
type Appended struct {
	// Path is the file that was written, with any symlink followed.
	Path string
	// Block is every byte that was appended, the blank line before it included.
	Block string
	// Regex is the rule as it went in.
	Regex string
}

// AppendIgnore adds an [[ignore]] table for regex to the end of the config file
// in dir, after a blank line and a comment saying when. Nothing else in the file
// changes. A missing file, and a missing dir, are created.
//
// It refuses rather than risk the config: when the file isn't a regular file,
// already has a problem, would go over MaxFileSize, or wouldn't load with the
// new table read back as exactly this regex. The file is replaced in one rename,
// so a crash part way leaves it as it was. If the file is a symlink, the file it
// points at is the one written and the link stays.
func AppendIgnore(dir, regex string, now time.Time) (Appended, error) {
	value, err := tomlString(regex)
	if err != nil {
		return Appended{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Appended{}, fmt.Errorf("making the config directory: %w", err)
	}
	path, err := resolve(Path(dir))
	if err != nil {
		return Appended{}, err
	}
	data, exists, err := readFile(path)
	if err != nil {
		return Appended{}, err
	}
	before := Default()
	if exists {
		if before, err = Parse(path, data); err != nil {
			return Appended{}, fmt.Errorf("sightline.toml already has a problem, so nothing was added. Fix it first: %w", err)
		}
	}

	var sep string
	switch {
	case len(data) == 0:
	case data[len(data)-1] == '\n':
		sep = "\n"
	default:
		sep = "\n\n"
	}
	block := fmt.Sprintf("%s# added from the feed on %s\n[[ignore]]\nregex = %s\n", sep, now.Format(time.DateOnly), value)
	next := append(bytes.Clone(data), block...)
	if len(next) > MaxFileSize {
		return Appended{}, &Error{File: path, Msg: "adding the rule would take the file over 1 MiB, so it wasn't added"}
	}

	// Read the result back the way the feed will. The new table has to come out
	// as one more ignore rule, the last, with exactly this regex. Anything else
	// means the file swallowed the block somehow, such as an unclosed string.
	after, err := Parse(path, next)
	if err != nil {
		return Appended{}, fmt.Errorf("the rule wasn't added because the file wouldn't load with it: %w", err)
	}
	if len(after.Ignores) != len(before.Ignores)+1 || after.Ignores[len(after.Ignores)-1].Regex.String() != regex {
		return Appended{}, &Error{File: path, Msg: "the rule wasn't added because it didn't read back as written"}
	}

	if err := replaceFile(path, data, exists, next); err != nil {
		return Appended{}, err
	}
	return Appended{Path: path, Block: block, Regex: regex}, nil
}

// ErrChanged says the file no longer ends with the table that was appended, so
// taking it out could remove something else.
var ErrChanged = errors.New("the file has changed since the rule was added")

// errRace says the file changed between reading it and replacing it.
var errRace = errors.New("sightline.toml changed while Sightline was writing it, so it was left alone. Try again")

// RemoveAppended takes a table AppendIgnore added back out, leaving the file as
// it was before. It only does so while the file still ends with exactly what was
// appended, and returns ErrChanged otherwise.
func RemoveAppended(a Appended) error {
	if a.Path == "" || a.Block == "" {
		return errors.New("nothing to undo")
	}
	data, exists, err := readFile(a.Path)
	if err != nil {
		return err
	}
	if !exists || !bytes.HasSuffix(data, []byte(a.Block)) {
		return ErrChanged
	}
	next := data[:len(data)-len(a.Block)]
	// Taking a whole table off the end can't break a file that loads, but check
	// anyway. A file that was already broken is left for the person to fix.
	if _, err := Parse(a.Path, data); err == nil {
		if _, err := Parse(a.Path, next); err != nil {
			return fmt.Errorf("the rule wasn't removed because the file wouldn't load without it: %w", err)
		}
	}
	return replaceFile(a.Path, data, true, next)
}

// tomlString writes s as a TOML string that reads back as exactly s. A literal
// string ('...') keeps a regex readable, since backslashes stay as they are. It
// can't hold a single quote or a control character other than tab, so those get
// a basic string ("...") with escapes instead. A rule is one line, so line
// breaks are refused rather than escaped.
func tomlString(s string) (string, error) {
	switch {
	case s == "":
		return "", errors.New("an ignore rule needs a regex")
	case !utf8.ValidString(s):
		return "", errors.New("the regex isn't valid UTF-8")
	case strings.ContainsAny(s, "\r\n"):
		return "", errors.New("an ignore rule has to be on one line")
	}
	literal := !strings.ContainsFunc(s, func(r rune) bool {
		return r == '\'' || (r < 0x20 && r != '\t') || r == 0x7f
	})
	if literal {
		return "'" + s + "'", nil
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

// resolve follows a symlink at path, so writing replaces the file it points at
// rather than the link. A path that doesn't exist yet is used as it is.
func resolve(path string) (string, error) {
	target, err := filepath.EvalSymlinks(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return path, nil
	case err != nil:
		return "", fmt.Errorf("finding the config file: %w", err)
	}
	return target, nil
}

// replaceFile writes data to path through a temporary file in the same directory
// and a rename, keeping the file's mode. old is what the file held when it was
// read. If that's changed in the meantime (an editor saved it), nothing is
// written, so someone else's edit isn't lost.
func replaceFile(path string, old []byte, existed bool, data []byte) error {
	mode := os.FileMode(0o600)
	if existed {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("writing config: %w", err)
		}
		mode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+FileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	// Until the rename, the temporary file is the only thing to clean up.
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	now, nowExists, err := readFile(path)
	if err != nil {
		return err
	}
	if nowExists != existed || !bytes.Equal(now, old) {
		return errRace
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	done = true
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir makes the rename itself durable where the system allows it. It's best
// effort: the file's contents are already synced, and some systems can't sync
// a directory at all.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	d.Close()
}
