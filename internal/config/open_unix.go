//go:build unix

package config

import "syscall"

// openFlags makes opening a FIFO return straight away instead of waiting for a
// writer. It has no effect on reading a regular file.
const openFlags = syscall.O_NONBLOCK
