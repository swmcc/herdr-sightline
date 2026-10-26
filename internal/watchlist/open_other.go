//go:build !unix

package watchlist

// openFlags adds nothing outside Unix. Windows support comes later (v0.4), and
// it has no FIFOs that block on open in the same way.
const openFlags = 0
