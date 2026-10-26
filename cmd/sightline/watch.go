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

// feedsDir holds one empty file per open feed, named after its pane, inside the
// state directory. It's how watch finds a feed to reuse and how a closing feed
// knows whether it was the last one. herdr's pane list doesn't say which panes
// belong to a plugin, so Sightline keeps track itself.
const feedsDir = "feeds"

// actionTimeout bounds a watch, so a herdr that stops answering can't leave
// the action hanging in herdr's log.
const actionTimeout = 10 * time.Second

// exitTimeout bounds the tidy up when a feed closes. The feed's own context is
// already cancelled by then.
const exitTimeout = 2 * time.Second

// watch puts the focused pane on the watch list and makes sure a feed is open
// in its workspace to watch it.
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
func watchPane(ctx context.Context, client *herdr.Client, stateDir, pane, plugin string) (string, error) {
	panes, err := client.ListPanes(ctx)
	if err != nil {
		return "", fmt.Errorf("listing panes: %w", err)
	}
	var workspace string
	alive := map[string]bool{}
	for _, p := range panes {
		alive[p.ID] = true
		if p.ID == pane {
			workspace = p.WorkspaceID
		}
	}
	if !alive[pane] {
		return "", fmt.Errorf("pane %s isn't open", pane)
	}

	feeds, err := openFeeds(stateDir, panes)
	if err != nil {
		return "", err
	}
	if _, ok := feeds[pane]; ok {
		return "", fmt.Errorf("%s is the Sightline feed, which can't watch itself", pane)
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

	for id, ws := range feeds {
		if ws != workspace {
			continue
		}
		if err := client.FocusPluginPane(ctx, id); err != nil {
			return "", fmt.Errorf("%s is on the watch list, but the feed in %s couldn't be focused: %w", pane, id, err)
		}
		return msg + " The feed is already open.", nil
	}

	id, err := client.OpenPluginPane(ctx, herdr.PluginPaneOpen{
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
	_ = registerFeed(stateDir, id)
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

// openFeeds returns the feeds that are open, as pane id to workspace id.
// Notes for feeds whose panes have gone (a feed that crashed, say) are removed
// on the way. herdr doesn't reuse pane ids, so a note can't point at the wrong
// pane.
func openFeeds(stateDir string, panes []herdr.Pane) (map[string]string, error) {
	entries, err := os.ReadDir(filepath.Join(stateDir, feedsDir))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking for open feeds: %w", err)
	}
	workspaces := make(map[string]string, len(panes))
	for _, p := range panes {
		workspaces[p.ID] = p.WorkspaceID
	}
	feeds := map[string]string{}
	for _, e := range entries {
		id := e.Name()
		ws, ok := workspaces[id]
		if !ok || !watchlist.ValidPaneID(id) {
			_ = os.Remove(filepath.Join(stateDir, feedsDir, id))
			continue
		}
		feeds[id] = ws
	}
	return feeds, nil
}

// registerFeed notes that the feed in pane is open.
func registerFeed(stateDir, pane string) error {
	if stateDir == "" || !watchlist.ValidPaneID(pane) {
		return nil
	}
	dir := filepath.Join(stateDir, feedsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("noting the feed in %s: %w", pane, err)
	}
	if err := os.WriteFile(filepath.Join(dir, pane), nil, 0o644); err != nil {
		return fmt.Errorf("noting the feed in %s: %w", pane, err)
	}
	return nil
}

// endFeed runs as a feed closes. It removes the feed's note and, when no other
// feed is open, clears the watch list. The README says closing the feed stops
// watching, so opening one again starts from nothing rather than quietly
// picking up panes watched an hour ago.
func endFeed(client *herdr.Client, stateDir, self string) error {
	if stateDir == "" {
		return nil
	}
	if watchlist.ValidPaneID(self) {
		err := os.Remove(filepath.Join(stateDir, feedsDir, self))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing the feed's note: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), exitTimeout)
	defer cancel()
	others := 0
	if panes, err := client.ListPanes(ctx); err == nil {
		feeds, err := openFeeds(stateDir, panes)
		if err != nil {
			return err
		}
		others = len(feeds)
	} else {
		// Without herdr to ask, any note left could be a feed still open, so
		// only clear the list when there are none.
		entries, err := os.ReadDir(filepath.Join(stateDir, feedsDir))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("looking for open feeds: %w", err)
		}
		others = len(entries)
	}
	if others > 0 {
		return nil
	}
	if err := watchlist.Write(stateDir, nil); err != nil {
		return fmt.Errorf("clearing the watch list: %w", err)
	}
	return nil
}
