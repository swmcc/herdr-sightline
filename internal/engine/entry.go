package engine

import "time"

// EntryID names one entry in the feed. It's stable for the life of the engine.
type EntryID string

// Entry is one caught error as the feed shows it.
type Entry struct {
	ID          EntryID
	Source      Source
	Fingerprint string   // same failure, same fingerprint (#7)
	Lines       []string // the error itself
	Before      []string // context (#8)
	After       []string
	Count       int
	FirstSeen   time.Time
	LastSeen    time.Time
	State       State
	Agent       string // set by MarkSent
	MaybeGap    bool   // Gap was called while this entry was being captured
}

// State is where an entry is in its life, from caught to a verdict.
type State int

const (
	StateNew State = iota
	StateSent
	StateFixed
	StateStillFailing
	StateUnverified
	StateDismissed
)

// String gives the state as a plain word, for logs and test failures.
func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateSent:
		return "sent"
	case StateFixed:
		return "fixed"
	case StateStillFailing:
		return "still failing"
	case StateUnverified:
		return "unverified"
	case StateDismissed:
		return "dismissed"
	}
	return "unknown"
}

// open reports whether the error is still live, so a repeat should add to this
// entry rather than start a new one.
func (s State) open() bool {
	return s == StateNew || s == StateSent || s == StateStillFailing
}

// clone copies an entry so callers can't reach the engine's slices.
func (en Entry) clone() Entry {
	en.Lines = append([]string(nil), en.Lines...)
	en.Before = append([]string(nil), en.Before...)
	en.After = append([]string(nil), en.After...)
	return en
}
