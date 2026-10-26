// Package feed is the Sightline feed: the pane that shows caught errors and,
// because herdr plugins can't run background daemons, the watcher too. Close
// it and watching stops.
//
// The feed never sends anything without a keypress. Enter on an entry checks
// the target agent is still there and not blocked, then makes exactly one
// agent.prompt call with the engine's message, byte for byte what v shows.
//
// How herdr 0.9.1 types a prompt of several lines, checked with a fake agent
// in a throwaway pane that printed every byte it read: the whole text arrives
// in one write, with its newlines as "\n", and then Enter arrives on its own as
// "\r". When the program has turned on bracketed paste (ESC[?2004h), as Claude
// Code and Codex do, the text comes wrapped in ESC[200~ and ESC[201~, so the
// agent sees one paste and the newlines inside it don't submit anything. A
// program without bracketed paste would see each "\n" as the end of a line.
// The engine strips escapes from pane output, so nothing in the message can
// end the paste early. herdr also refuses agent.prompt to a blocked agent
// (agent_blocked) and to a pane whose agent is no longer its foreground
// process, which backs up the feed's own check.
package feed

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swmcc/herdr-sightline/internal/config"
	"github.com/swmcc/herdr-sightline/internal/herdr"
)

// Options configure Run.
type Options struct {
	Client *herdr.Client
	// ConfigDir holds sightline.toml (HERDR_PLUGIN_CONFIG_DIR). Empty means the
	// defaults, never reloaded.
	ConfigDir string
	// StateDir holds the watch list (HERDR_PLUGIN_STATE_DIR). Empty means only
	// Watch is watched.
	StateDir string
	// Watch are panes to watch on top of the watch list, from --watch.
	Watch []string
	// Self is the feed's own pane (HERDR_PANE_ID). It's never watched.
	Self string
	// Now is the clock. Nil means time.Now. It's called from several
	// goroutines.
	Now func() time.Time
	// Every is how often the loops wake up.
	Every Timings
	// Program are extra Bubble Tea options, such as the alt screen or, in
	// tests, input and output.
	Program []tea.ProgramOption
}

// Run shows the feed until the person quits or ctx ends. Every goroutine it
// starts has stopped by the time it returns.
func Run(ctx context.Context, opts Options) error {
	if opts.Client == nil {
		return errors.New("feed: no herdr client")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	every := opts.Every.withDefaults()

	first := config.Update{Config: config.Default()}
	var updates <-chan config.Update
	if opts.ConfigDir != "" {
		// Watch sends the file as it is straight away, then each change.
		updates = config.Watch(ctx, opts.ConfigDir, every.Config)
		select {
		case first = <-updates:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	m := newModel(ctx, opts.Client, now, opts.Self, first)
	p := tea.NewProgram(m, append([]tea.ProgramOption{tea.WithContext(ctx)}, opts.Program...)...)
	l := newLoops(ctx, opts.Client, p.Send, now, every)
	m.loops = l
	l.startShared(opts.StateDir, opts.Watch, updates)

	_, err := p.Run()
	cancel()
	l.wait()
	if errors.Is(err, tea.ErrProgramKilled) {
		// The context ended, which is how a caller asks the feed to stop.
		return nil
	}
	return err
}
