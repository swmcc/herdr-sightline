// Package injection is Sightline's prompt injection suite (#15). It has no code
// of its own, only tests.
//
// The tests play hostile pane output through the real path, from what herdr's
// pane read would return (or a raw stream, for the escape cases) through the
// watcher's diff and the engine to the exact message an agent would get. Every
// message is read by one strict parser that knows the format from the README,
// not from the engine's code, so a change to the engine can't quietly change
// what the tests accept.
//
// Queue mode's batches of 2 to 5 errors go through the same corpus by way of
// AutoSend and are read by a batch parser held to the same rules. See
// batch_test.go.
//
// The corpus lives in internal/engine/testdata/injection. See corpus_test.go for
// its format.
package injection
