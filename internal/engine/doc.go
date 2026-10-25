// Package engine is the part of Sightline that knows nothing about herdr.
//
// It takes lines of output in and gives entries and messages out: detection,
// grouping, the fix check and the message sent to agents all live here.
//
// It uses the Go standard library only and must never import the herdr adapter.
// That boundary is what lets Sightline outlive any one tool. See #3.
package engine
