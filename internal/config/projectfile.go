package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// FindRoot finds the repo dir is in: the nearest directory at or above it, once
// symlinks are resolved, that holds a .git (a directory, or a file in a
// worktree or submodule). ok is false when there's none, or when dir has gone.
//
// dir is a pane's working directory, which whatever runs in the pane can
// change, so the answer is only ever where to look. Nothing found there is used
// until the person trusts it.
func FindRoot(dir string) (root string, ok bool, err error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return "", false, nil
	}
	real, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("finding the repo for %s: %w", dir, err)
	}
	for d := real; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, true, nil
		}
		if parent := filepath.Dir(d); parent == d {
			return "", false, nil
		}
	}
}

// ProjectPath is the project file for the repo at root.
func ProjectPath(root string) string { return filepath.Join(root, ProjectFileName) }

// ReadProject reads root's project file. A missing file isn't an error. It
// refuses a symlink, even one that points inside the repo, so a file outside
// the repo can never pass for the repo's own, and anything that isn't a regular
// file or is over MaxProjectFileSize.
func ReadProject(root string) (data []byte, exists bool, err error) {
	path := ProjectPath(root)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, true, &Error{File: path, Msg: "is a symlink, and a project file has to be a real file in the repo"}
	}
	if !info.Mode().IsRegular() {
		return nil, true, notRegular(path, info)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openFlags|noFollow, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		// Swapped for a symlink since the Lstat, most likely.
		return nil, true, &Error{File: path, Msg: fmt.Sprintf("couldn't be opened as a real file: %v", err)}
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, true, fmt.Errorf("reading %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, true, notRegular(path, info)
	}
	data, err = io.ReadAll(io.LimitReader(f, MaxProjectFileSize+1))
	if err != nil {
		return nil, true, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) > MaxProjectFileSize {
		return nil, true, &Error{File: path, Msg: fmt.Sprintf("file is bigger than %d KiB, which is far too big for a project file", MaxProjectFileSize>>10)}
	}
	return data, true, nil
}
