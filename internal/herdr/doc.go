// Package herdr is Sightline's adapter for herdr.
//
// It talks to herdr's socket, reads the watched panes and hands new lines to the
// engine, subscribes to agent status and pane events and delivers messages to
// agents. It feeds the engine and carries out what the
// engine decides. The engine never imports this package.
package herdr
