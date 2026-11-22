# How it works

The full rules behind the README's short version: which agent gets an error, what
the message looks like and how Sightline decides a fix worked.

## Which agent gets the error

By default, the most recently active agent **in the same tab** as the watched pane.
That's almost always the one that just changed the code. An agent counts as active
when it starts working. Sightline asks rather than guessing when:

- there's no agent in the tab at all
- every agent in the tab is blocked, which usually means a permission dialog
- more than one agent could be the one and none has started work since Sightline
  started

A blocked agent is never picked by default. Sightline never quietly picks an agent
from another tab.

Press `t` on an entry to pick a different agent. To pin a pane to an agent:

```toml
# sightline.toml
[[watch]]
pane_title = "web"
agent = "claude"
```

`pane_title` must match the pane's label exactly. A pane with no label is matched on
its terminal title instead. `agent` is matched, ignoring case, against the agent's
herdr name (`herdr agent rename`), then the label on its pane, then its kind, such as
`claude` or `codex`. A herdr name is unique, so it can pin an agent in another tab. A
label or kind only counts in the watched pane's tab. If it only fits an agent
somewhere else, Sightline asks. If the pinned agent isn't running at all, Sightline
falls back to the tab and says so.

Sending to an agent in another tab always takes a second `enter`, unless you
picked it yourself with `t`. A pane with no label can set its own terminal title,
so a pin that matches the title isn't enough on its own to send that far.

## The message

Nothing the pane can influence appears outside the block. The header names the
pane only by herdr's pane id. The pane's name and the command it runs can be set
by whatever runs in it, so they go inside the block with its output.

Every line inside the block starts with `|`, and none of Sightline's own lines do.
Output that prints something like the end of the message followed by a new one
still reads as pane output, because every line of it starts with `|`. Output
lines start with `| ` and the pane's name and command with `|# `, so output can't
pass for those either. The header says what the block holds and how many lines of
output there are, and the last line says where they end.

The closing line ("Please investigate and fix the cause.") is configurable with
`closing_line` (see [Configuration](configuration.md)). It's kept to one line of at
most 200 characters, with backticks turned into apostrophes.

Each line is cut at 500 characters and the whole message at 16 KiB, so a long
stack trace or a pane full of noise can't flood the agent. When the cap bites,
the context furthest from the error goes first, then the end of the error itself,
and the header says how many lines were left out.

[Queue mode](queue-mode.md#several-errors-in-one-message) can put several errors in
one message, each in its own block.

## The fix check

After an error is sent, Sightline waits for the agent to finish, then watches the
pane for a settle period (30 seconds by default).

"Fixed" means *the error stopped appearing*, not *the code is correct*. Quiet isn't
proof, so `✓ fixed` needs the pane to show something ran after the agent finished,
in the first half of the settle period so the error has had time to come back:

- A compile, build, restart or test run finishing, like `compiled successfully`,
  `ready in 312 ms`, `Tests: 4 passed` or `* Listening on`. It only counts at the
  start of its line, after at most a symbol, a time, tags with no spaces, a log
  level and the tool's name. Request logs don't count. They only show the server
  is up. Nothing on a line over 4 KiB counts, as only its head is read.
- If the error came with a request (`GET /notices?page=2 500`), that same
  request again, query and all, with a 2xx. A redirect or a 404 doesn't count,
  because anyone can send the same request in a way that never reaches the bug.
  Where the server logs the request before its status (Rails' `Started` and
  `Completed`), the next request to end has to be that one. It only counts in a
  request line's own form, so a request someone puts in a query string doesn't.
  A recompile doesn't prove a request bug is gone. If Sightline can't tell which
  of several requests the error came from, it says `? unverified`. With Rails or
  Phoenix that's whenever two requests, or a request and an ending before it,
  are logged near the error, even after a recompile, because their logs don't
  say which request each `Completed` ends.

A request only counts as the error's when the app answered it with a 5xx or it
was still being handled when the error struck. A bug that only shows for one
user, one session or one header can still look fixed when someone else's same
request gets a 2xx. Request logs that end with a referer and user agent (the
combined format of nginx, morgan and gunicorn) aren't read, so an error from one of
those stays `? unverified` unless something recompiles.

A dev server that restarts on save (`node --watch`, nodemon) prints its restart
while the agent is still working, so it doesn't count. An error at startup
usually ends `? unverified`. That's [#90](https://github.com/swmcc/herdr-sightline/issues/90).

If Sightline may have missed output from the pane during the settle period, or
the pane stops being watched, it says `? unverified` whatever else it saw.

If the agent goes away before it finishes (its pane closes or exits, or another
agent takes the pane over), there's nothing left to wait for. The entry says
`? unverified` with the reason, and `enter` sends it again to whichever agent is
chosen then. Once the agent has finished, it going away changes nothing, because
the verdict comes from the pane, not the agent.
