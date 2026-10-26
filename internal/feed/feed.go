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
	"fmt"
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

// Run shows the feed until the person quits or ctx ends. Every loop it starts,
// the config watch included, has stopped by the time it returns. A check or send
// still in flight ends with the context a moment later.
//
// A panic in the feed's own code (a loop, a command, the update or the view)
// stops it. The root context is cancelled, Bubble Tea puts the terminal back,
// every loop is waited for and Run returns the panic, with its stack, as an
// error. Goroutines that other packages start for the feed, config.Watch's and
// the herdr client's subscriptions, aren't covered: a panic there still kills
// the process.
func Run(parent context.Context, opts Options) (err error) {
	if opts.Client == nil {
		return errors.New("feed: no herdr client")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	f := &faults{ctx: ctx, cancel: cancel}
	var (
		l       *loops
		updates <-chan config.Update
	)
	// Run's own goroutine can panic too, before Bubble Tea is running to catch
	// it (building the first engine, say). Stop whatever has started and
	// return the panic like any other.
	defer func() {
		if r := recover(); r != nil {
			cancel()
			if l != nil {
				l.wait()
			} else if updates != nil {
				// No config loop is reading yet, so wait for Watch here.
				for range updates {
				}
			}
			err = fmt.Errorf("feed: %w", newPanicError("starting the feed", r))
		}
	}()
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	every := opts.Every.withDefaults()

	first := config.Update{Config: config.Default()}
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
	m.configDir = opts.ConfigDir
	// Bubble Tea's own panic catch stays on. The guard gets there first for
	// everything the feed runs, but Bubble Tea's catch still covers Bubble Tea
	// itself and restores the terminal if that ever panics.
	p := tea.NewProgram(&guarded{model: m, faults: f}, append([]tea.ProgramOption{tea.WithContext(ctx)}, opts.Program...)...)
	l = newLoops(ctx, f, opts.Client, p.Send, now, every)
	m.loops = l
	l.startShared(opts.StateDir, opts.Watch, updates)

	_, err = p.Run()
	// Bubble Tea wraps every reason the loop ends (a panic, a broken input
	// as well as a cancelled context) in ErrProgramKilled. Only the caller's
	// own cancel is a clean stop.
	stopped := parent.Err() != nil
	cancel()
	l.wait()
	// A panic cancels the root, which is also what tells Bubble Tea to stop,
	// so it's checked first. Checking after the wait catches a loop that
	// panicked on its way out.
	if fault := f.err(); fault != nil {
		// A send still in flight when something else panicked may have been
		// typed, but its result was dropped with the model. p.Run has
		// returned, so nothing else touches the model now.
		if n := len(m.sending); n > 0 {
			return fmt.Errorf("feed: %w\n\n%d send(s) were under way, so check whether they reached their agents", fault, n)
		}
		return fmt.Errorf("feed: %w", fault)
	}
	if stopped {
		return nil
	}
	if err != nil {
		return fmt.Errorf("feed: %w", err)
	}
	return nil
}

// guarded is the model as Bubble Tea sees it. It catches a panic in Update or
// View before Bubble Tea does, so Run gets the panic and its stack back rather
// than Bubble Tea printing them over the screen. It also wraps every command
// the model returns, so a panic in one is caught the same way.
//
// After a panic the model may be halfway through a change, so it isn't
// touched again. The root context is already cancelled, and Bubble Tea stops on
// its next turn and restores the terminal as it goes.
type guarded struct {
	model  *Model
	faults *faults
}

func (g *guarded) Init() tea.Cmd {
	defer g.faults.catch("the feed's init")
	return g.faults.guard("the feed's first command", g.model.Init())
}

func (g *guarded) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if g.faults.err() != nil {
		return g, nil
	}
	var cmd tea.Cmd
	func() {
		defer g.faults.catch("the feed's update")
		panicPoint(g.faults.ctx, "update")
		_, cmd = g.model.Update(msg)
	}()
	if sm, ok := msg.(sentMsg); ok {
		// A panic in the send comes back as an uncertain send, so the model
		// has now flagged the entry as maybe sent. That flag goes when the
		// feed stops, so the error says it as well.
		var pe *panicError
		if errors.As(sm.promptErr, &pe) {
			g.faults.fail(fmt.Errorf("sending to %s, which may or may not have reached it: %w", clean(sm.checked.target.Label), pe))
			return g, nil
		}
	}
	return g, g.faults.guard("a command", cmd)
}

func (g *guarded) View() string {
	if g.faults.err() != nil {
		return ""
	}
	defer g.faults.catch("the feed's view")
	panicPoint(g.faults.ctx, "view")
	return g.model.View()
}
