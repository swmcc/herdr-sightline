//go:build unix

package config

import "syscall"

// openFlags makes opening a FIFO return straight away instead of waiting for a
// writer. It has no effect on reading a regular file.
const openFlags = syscall.O_NONBLOCK

// noFollow makes opening a symlink fail rather than open what it points at.
// A project file is checked with Lstat first, and this closes the gap after.
const noFollow = syscall.O_NOFOLLOW
