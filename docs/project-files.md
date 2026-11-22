# Project files

A repo can carry its own `.sightline.toml` at its root, so everyone working on it
gets the same patterns and ignore rules:

```toml
# .sightline.toml, next to .git
presets = ["go", "node"]

[[pattern]]
name = "our-framework"
regex = '^FATAL \[app\] .+'

# A known flaky upstream that isn't ours to fix.
[[ignore]]
regex = '^Error: flaky upstream timed out'
```

Sightline finds it from each watched pane's working directory, going up to the
nearest directory with a `.git` in it. It only applies to watched panes in that repo,
and only once you've trusted it. Until then the feed says it's there and ignores it.

## What it can set

It can set `presets`, `[[pattern]]` and `[[ignore]]` and nothing else. Your own
`sightline.toml` wins wherever they differ:

- Its patterns and ignore rules are added to yours, never in place of them.
- Its presets can only switch presets on, and only if your file doesn't list
  `presets`. If yours does, that list is final.
- Your ignore rules are checked first. Its ignore rules never dismiss an entry that's
  already in the feed.

It has to be a real file (not a symlink) of at most 64 KiB, with at most 50 patterns
and 50 ignore rules, each regex at most 500 bytes. Its ignore rules are held tighter
than yours, so each one names the error it hides (see below). If anything in it is
wrong, none of it applies and the feed says why.

When one of its ignore rules hides a line that would have been an error, the feed
counts it in the header and `h` lists what was hidden. If that happens while a fix is
being checked in that pane, the result is `? unverified`, never `✓ fixed`.

An error only a project file's pattern caught never goes on its own in
[queue mode](queue-mode.md). It waits for `enter`.

## Trusting it

**A project file is untrusted too.** Whoever can push to a repo writes its
`.sightline.toml`, so cloning one mustn't change what Sightline does. Until you trust
it, the feed says it's there and ignores it. Press `p` to read it. Each rule is
shown quoted, with anything outside plain ASCII escaped. `y` only works once
you've scrolled to the end, so every rule has been on screen. Press `y` to trust that
version or `esc` to leave it. `x` stops trusting it.

- It can only add presets, error patterns and ignore rules. Its presets can only
  switch presets on. It can't change what's sent, which agent gets it or when, so it
  can never turn on queue mode or pin a pane to an agent. A file that tries is
  refused whole.
- Trust is kept in Sightline's state directory, which a repo can't write to, against
  the repo's real path and a sha256 of the file's exact bytes. The rules that apply
  are read from the same bytes that were hashed, so a file swapped after you trusted
  it is just a new version that isn't trusted. If it changes while you're reading
  it, the screen says so and starts again from the top.
- A rule that would match an empty line or ordinary output is refused, and so is an
  ignore rule that matches the start of a common error (`Error: `, `panic: `, a
  Python traceback and so on) followed by any message, with or without a process
  manager label in front. A regex with a run of spaces in it is refused too, since
  spaces could push the rest of it off the screen.
- A project's ignore rule has to start with `^` and fixed text that names an error
  and at least 12 characters of its message, like `^Error: flaky upstream`. A
  process manager label at the start doesn't count towards that. It can't
  match anywhere in a line or start with a class, an alternation or `(?i)`. Nor can
  it name only a process manager label. Those are the ways a rule hides a whole
  kind of error. Your own ignore rules have no such limit.
- Those checks catch the obvious ways to hide errors, not every one. An ignore rule
  from a repo can still hide a real error, and the trust screen says so. Read the
  rules before you press `y`.

To share a pattern with everyone, see
[Share an error pattern](../CONTRIBUTING.md#share-an-error-pattern).
