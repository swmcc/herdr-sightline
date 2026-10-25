// Package herdr is Sightline's adapter for herdr.
//
// It talks to herdr's socket, subscribes to pane output and agent status, and
// delivers messages to agents. It feeds the engine and carries out what the
// engine decides. The engine never imports this package.
package herdr
