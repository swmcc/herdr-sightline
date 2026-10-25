// Command sightline is the single binary behind the Sightline herdr plugin.
// herdr calls it with a subcommand for each action and pane in herdr-plugin.toml.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Sightline: your agents can see the panes next to them.

Usage:
  sightline <command>

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
		return feed(stdin, stdout)
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

// feed is a placeholder for the real feed (#9). It stays open until enter is
// pressed so the pane doesn't flash and vanish.
func feed(stdin io.Reader, stdout io.Writer) int {
	fmt.Fprintf(stdout, "Sightline %s\n\nThe feed isn't built yet, see #9.\nPress enter to close.\n", version)
	_, _ = bufio.NewReader(stdin).ReadString('\n')
	return 0
}
