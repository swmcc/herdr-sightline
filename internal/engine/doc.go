// Package engine is the part of Sightline that knows nothing about herdr.
//
// It takes lines of output in and gives entries and messages out: detection,
// grouping, the fix check and the message sent to agents all live here.
//
// It uses the Go standard library only and must never import the herdr adapter.
// That boundary is what lets Sightline outlive any one tool. See #3.
//
// # Using it
//
// An adapter makes one [Engine] with [New] and calls it from one goroutine. The
// engine is synchronous, owns no goroutines and never reads the clock: every call
// that cares about time is given it.
//
//   - [Engine.Watch] and [Engine.Unwatch] say which sources to listen to.
//   - [Engine.Feed] hands over new lines from a source. [Engine.Gap] says some
//     output may have been missed and [Engine.Unsure] says a read couldn't tell
//     what was new. Both come before the lines from the same read. After a
//     reconnect, call Gap rather than Unwatch and Watch.
//   - [Engine.Rename] says a source now goes by another id, as a pane does
//     when it moves. Its entries and their fix check go with it.
//   - [Engine.Tick] moves the capture wait and the settle period on, so call it
//     regularly even when nothing is printed.
//   - [Engine.Entries] is what the feed shows, newest first.
//   - [Engine.MarkSent], [Engine.AgentFinished] and [Engine.Dismiss] record what
//     the person and the agent did.
//   - [Engine.Message] is exactly what gets sent to an agent, byte for byte.
//
// Each concern has its own file: detect.go (#6), group.go (#7), window.go (#8),
// message.go (#11) and fixcheck.go (#12). Each starts as the simplest thing that
// works and is replaced by its issue without changing the interface.
//
// Package enginetest has a fake adapter with a fake clock for tests.
package engine
