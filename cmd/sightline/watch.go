package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/swmcc/herdr-sightline/internal/herdr"
	"github.com/swmcc/herdr-sightline/internal/watchlist"
)

// pluginID is Sightline's id in herdr-plugin.toml, used when herdr doesn't
// pass HERDR_PLUGIN_ID.
const pluginID = "swmcc.sightline"

// feedEntrypoint is the feed's pane id in herdr-plugin.toml.
const feedEntrypoint = "feed"

// feedsDir holds one empty file per open feed inside the state directory. It's
// how watch finds the feed to reuse and how a closing feed knows whether it was
// the last one. herdr's pane list doesn't say which panes belong to a plugin,
// so Sightline keeps track itself.
//
// Each file is named after the feed's terminal id rather than its pane id,
// because a pane that moves to another workspace gets a new pane id but keeps
// its terminal.
const feedsDir = "feeds"

// actionTimeout bounds a watch, so a herdr that stops answering can't leave
// the action hanging in herdr's log.
const actionTimeout = 10 * time.Second

// exitTimeout bounds asking herdr about other feeds when a feed closes.
const exitTimeout = 2 * time.Second

// watch puts the focused pane on the watch list and makes sure a feed is open
// to watch it.
func watch(getenv func(string) string, stdout, stderr io.Writer) int {
	pane, stateDir, err := actionTarget(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "sightline watch: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()

	client, err := dial(ctx, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "sightline watch: %v\n", err)
		return 1
	}
	msg, err := watchPane(ctx, client, stateDir, pane, pluginIDFrom(getenv))
	if err != nil {
		fmt.Fprintf(stderr, "sightline watch: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// watchPane does the work of watch and returns what to tell the user.
//
// There's only ever one feed. Every feed reads the whole watch list, so a
// second one would show each error twice and could send it twice. If a feed is
// open anywhere it's focused, even in another workspace, so it's clear where
// the errors will go.
func watchPane(ctx context.Context, client *herdr.Client, stateDir, pane, plugin string) (string, error) {
	panes, err := client.ListPanes(ctx)
	if err != nil {
		return "", fmt.Errorf("listing panes: %w", err)
	}
	var target herdr.Pane
	alive := map[string]bool{}
	for _, p := range panes {
		alive[p.ID] = true
		if p.ID == pane {
			target = p
		}
	}
	if target.ID == "" {
		return "", fmt.Errorf("pane %s isn't open", pane)
	}

	feeds, err := openFeeds(stateDir, panes)
	if err != nil {
		return "", err
	}
	for _, f := range feeds {
		if f.ID == pane {
			return "", fmt.Errorf("%s is the Sightline feed, which can't watch itself", pane)
		}
	}
	// With no feed open the list is only ids waiting for the next feed, so the
	// ones for closed panes can go. With a feed open they stay, because the feed
	// follows a pane that moved by its old id.
	if len(feeds) == 0 {
		if err := pruneWatchList(stateDir, alive); err != nil {
			return "", err
		}
	}

	added, err := watchlist.Add(stateDir, pane)
	if err != nil {
		return "", err
	}
	msg := "Watching " + pane + "."
	if !added {
		msg = "Already watching " + pane + "."
	}

	// The feed in the pane's own workspace comes first. One that can't be
	// focused (it closed a moment ago, say) is passed over.
	for _, sameWorkspace := range []bool{true, false} {
		for _, f := range feeds {
			if (f.WorkspaceID == target.WorkspaceID) != sameWorkspace {
				continue
			}
			if client.FocusPluginPane(ctx, f.ID) == nil {
				return msg + " The feed is already open in " + f.ID + ".", nil
			}
		}
	}

	opened, err := client.OpenPluginPane(ctx, herdr.PluginPaneOpen{
		PluginID:   plugin,
		Entrypoint: feedEntrypoint,
		Placement:  "split",
		TargetPane: pane,
		Direction:  "right",
		Focus:      true,
	})
	if err != nil {
		return "", fmt.Errorf("%s is on the watch list, but the feed couldn't be opened: %w", pane, err)
	}
	// The feed notes itself as it starts. Doing it here as well closes the gap
	// before it does, so a quick second watch can't open another. If this
	// fails the feed's own note still covers it, so it's not worth failing a
	// watch that worked.
	_ = registerFeed(stateDir, opened.TerminalID)
	return msg + " Opened the feed beside it.", nil
}

// unwatch takes the focused pane off the watch list. The feed notices within a
// second and stops watching it.
func unwatch(getenv func(string) string, stdout, stderr io.Writer) int {
	pane, stateDir, err := actionTarget(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "sightline unwatch: %v\n", err)
		return 1
	}
	removed, err := watchlist.Remove(stateDir, pane)
	if err != nil {
		fmt.Fprintf(stderr, "sightline unwatch: %v\n", err)
		return 1
	}
	if !removed {
		fmt.Fprintf(stdout, "%s wasn't being watched.\n", pane)
		return 0
	}
	fmt.Fprintf(stdout, "Stopped watching %s.\n", pane)
	return 0
}

// actionTarget is the focused pane and the state directory for an action.
func actionTarget(getenv func(string) string) (pane, stateDir string, err error) {
	pane = focusedPane(getenv)
	if pane == "" {
		return "", "", errors.New("herdr didn't say which pane is focused (HERDR_PANE_ID is empty)")
	}
	if !watchlist.ValidPaneID(pane) {
		return "", "", fmt.Errorf("%q isn't a pane id", pane)
	}
	stateDir = getenv(watchlist.StateDirEnv)
	if stateDir == "" {
		return "", "", fmt.Errorf("%s is not set, so there's nowhere to keep the watch list (is this running inside herdr?)", watchlist.StateDirEnv)
	}
	return pane, stateDir, nil
}

// focusedPane is the pane an action was run on. herdr passes it as
// HERDR_PANE_ID, and in HERDR_PLUGIN_CONTEXT_JSON as a fallback.
func focusedPane(getenv func(string) string) string {
	if pane := getenv("HERDR_PANE_ID"); pane != "" {
		return pane
	}
	var ctx struct {
		FocusedPaneID string `json:"focused_pane_id"`
	}
	if json.Unmarshal([]byte(getenv("HERDR_PLUGIN_CONTEXT_JSON")), &ctx) != nil {
		return ""
	}
	return ctx.FocusedPaneID
}

func pluginIDFrom(getenv func(string) string) string {
	if id := getenv("HERDR_PLUGIN_ID"); id != "" {
		return id
	}
	return pluginID
}

// pruneWatchList drops panes that are no longer open from the list, so it
// doesn't grow without end. A list that can't be read is left alone for
// watchlist.Add to report.
func pruneWatchList(stateDir string, alive map[string]bool) error {
	listed, err := watchlist.Read(stateDir)
	if err != nil {
		return nil
	}
	var keep []string
	for _, id := range listed {
		if alive[id] {
			keep = append(keep, id)
		}
	}
	if len(keep) == len(listed) {
		return nil
	}
	return watchlist.Write(stateDir, keep)
}

// selfTerminal is the terminal id of the pane the feed runs in, or empty if
// herdr can't say.
func selfTerminal(ctx context.Context, client *herdr.Client, self string) string {
	if self == "" {
		return ""
	}
	panes, err := client.ListPanes(ctx)
	if err != nil {
		return ""
	}
	for _, p := range panes {
		if p.ID == self {
			return p.TerminalID
		}
	}
	return ""
}

// openFeeds returns the panes of the feeds that are open. Notes for feeds that
// have gone (one that crashed, say) are removed on the way. herdr doesn't reuse
// ids, so a note can't point at the wrong pane.
func openFeeds(stateDir string, panes []herdr.Pane) ([]herdr.Pane, error) {
	dir := filepath.Join(stateDir, feedsDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking for open feeds: %w", err)
	}
	byTerminal := make(map[string]herdr.Pane, len(panes))
	for _, p := range panes {
		if p.TerminalID != "" {
			byTerminal[p.TerminalID] = p
		}
	}
	var feeds []herdr.Pane
	for _, e := range entries {
		p, ok := byTerminal[e.Name()]
		if !ok {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		feeds = append(feeds, p)
	}
	return feeds, nil
}

// validTerminalID reports whether id is safe to use as a file name. herdr's
// look like "term_65eb4db4ed59f65", which fits the same rules as a pane id.
func validTerminalID(id string) bool {
	return watchlist.ValidPaneID(id)
}

// registerFeed notes that the feed in terminal is open.
func registerFeed(stateDir, terminal string) error {
	if stateDir == "" || !validTerminalID(terminal) {
		return nil
	}
	dir := filepath.Join(stateDir, feedsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("noting the feed in %s: %w", terminal, err)
	}
	if err := os.WriteFile(filepath.Join(dir, terminal), nil, 0o644); err != nil {
		return fmt.Errorf("noting the feed in %s: %w", terminal, err)
	}
	return nil
}

// endFeed runs as a feed closes. It removes the feed's note and, when no other
// feed is open, clears the watch list. The README says closing the feed stops
// watching, so opening one again starts from nothing rather than quietly
// picking up panes watched an hour ago.
func endFeed(client *herdr.Client, stateDir, terminal string) error {
	if stateDir == "" {
		return nil
	}
	dir := filepath.Join(stateDir, feedsDir)
	if validTerminalID(terminal) {
		err := os.Remove(filepath.Join(dir, terminal))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing the feed's note: %w", err)
		}
	}

	// herdr kills the process soon after it closes the pane, too soon to ask
	// it anything (seen on herdr 0.9.1). So with no other note the list is
	// cleared straight away, and herdr is only asked when there is one.
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("looking for open feeds: %w", err)
	}
	if len(entries) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), exitTimeout)
		defer cancel()
		panes, err := client.ListPanes(ctx)
		if err != nil {
			// Without herdr to ask, the other feed could still be open.
			return nil
		}
		feeds, err := openFeeds(stateDir, panes)
		if err != nil {
			return err
		}
		if len(feeds) > 0 {
			return nil
		}
	}
	if err := watchlist.Write(stateDir, nil); err != nil {
		return fmt.Errorf("clearing the watch list: %w", err)
	}
	return nil
}
