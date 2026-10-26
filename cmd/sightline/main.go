// Command sightline is the single binary behind the Sightline herdr plugin.
// herdr calls it with a subcommand for each action and pane in herdr-plugin.toml.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	sightlinefeed "github.com/swmcc/herdr-sightline/internal/feed"
	"github.com/swmcc/herdr-sightline/internal/herdr"
	"github.com/swmcc/herdr-sightline/internal/watchlist"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Sightline: your agents can see the panes next to them.

Usage:
  sightline <command>
  sightline feed [--watch PANE]...

Commands:
  watch     Start watching the focused pane
  unwatch   Stop watching the focused pane
  feed      Run the Sightline feed (herdr opens this as a pane)
  version   Print the version
  help      Show this help
`

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

// run is main without the process bits, so it can be tested.
func run(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	switch args[0] {
	case "watch":
		return notBuiltYet(stdout, "watch", "#5", getenv("HERDR_PANE_ID"))
	case "unwatch":
		return notBuiltYet(stdout, "unwatch", "#5", getenv("HERDR_PANE_ID"))
	case "feed":
		return feed(args[1:], getenv, stdin, stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "sightline", version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "sightline: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// notBuiltYet proves herdr is wiring actions through correctly until the real
// behaviour lands.
func notBuiltYet(stdout io.Writer, command, issue, pane string) int {
	if pane == "" {
		pane = "(no pane in context)"
	}
	fmt.Fprintf(stdout, "sightline %s: called for pane %s. Not built yet, see %s.\n", command, pane, issue)
	return 0
}

// paneList collects --watch flags.
type paneList []string

func (p *paneList) String() string { return strings.Join(*p, ",") }

func (p *paneList) Set(v string) error {
	for _, id := range strings.Split(v, ",") {
		id = strings.TrimSpace(id)
		if !watchlist.ValidPaneID(id) {
			return fmt.Errorf("%q isn't a pane id", id)
		}
		*p = append(*p, id)
	}
	return nil
}

// feed runs the feed, which is also the watcher. It watches the panes in the
// watch list in HERDR_PLUGIN_STATE_DIR and any given with --watch.
func feed(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("feed", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var panes paneList
	flags.Var(&panes, "watch", "a pane id to watch as well as the watch list (repeat it, or separate ids with commas)")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	// herdr sends SIGHUP when the pane closes. Bubble Tea handles SIGINT and
	// SIGTERM itself.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGHUP)
	defer stop()

	client, err := dial(ctx, getenv)
	if err != nil {
		// Stay open so the message can be read before the pane closes.
		fmt.Fprintf(stdout, "Sightline %s\n\nThe feed can't start: %v\nPress enter to close.\n", version, err)
		_, _ = bufio.NewReader(stdin).ReadString('\n')
		return 1
	}

	err = sightlinefeed.Run(ctx, sightlinefeed.Options{
		Client:    client,
		ConfigDir: getenv("HERDR_PLUGIN_CONFIG_DIR"),
		StateDir:  getenv(watchlist.StateDirEnv),
		Watch:     panes,
		Self:      getenv("HERDR_PANE_ID"),
		Program:   []tea.ProgramOption{tea.WithAltScreen()},
	})
	if err != nil {
		fmt.Fprintf(stderr, "sightline feed: %v\n", err)
		return 1
	}
	return 0
}

// dial connects to the herdr named in the environment.
func dial(ctx context.Context, getenv func(string) string) (*herdr.Client, error) {
	path := getenv(herdr.SocketEnv)
	if path == "" {
		return nil, fmt.Errorf("%s is not set, so herdr can't be reached (is this running inside herdr?)", herdr.SocketEnv)
	}
	return herdr.Dial(ctx, path)
}
