# Changelog

What changed in each version of Sightline. The full notes for each are on its
[release page](https://github.com/swmcc/herdr-sightline/releases).

## 0.3.0 · Queue mode and project presets (2026-11-22)

This one covers two milestones, v0.2 and v0.3, so there's no 0.2.0.

### Added

- **Queue mode.** Press `m` on an entry and agree on the screen that explains it, and
  that pane's errors go to the agent on their own once it's idle. It only sends to an
  agent Sightline is sure of, never while the agent is working or its pane has focus,
  holds back anything it isn't sure about and is capped. See
  [docs/queue-mode.md](docs/queue-mode.md).
- **Queue management in the feed.** `x` takes an entry out of the queue, `[` and `]`
  reorder it, `s` pauses everything, `a` lists what went and `v` shows a batch exactly
  as it will go.
- **Project files.** A repo can check in a `.sightline.toml` with presets, patterns and
  ignore rules. It does nothing until you read and trust it in the feed, and any change
  asks again. See [docs/project-files.md](docs/project-files.md).
- **New presets** for PHP, Elixir, .NET and Swift. The `tests` preset gains ExUnit,
  PHPUnit, `artisan test` and `dotnet test`.
- A demo clip in the README, and this changelog.

### Changed

- `context_lines` over 200 is now refused instead of quietly cut.
- The README is shorter, with the full rules moved to [docs/](docs/).

### Fixed

- Output drawn over on a raw stream (cursor moves, redraws, other character sets) now
  flags the entry as possibly missing output, rather than reaching the agent unseen.

## 0.1.0 · First light (2026-11-22)

The first release. Watch a pane, catch errors, see them in the feed, send one to an
agent with a keypress and find out whether the fix worked.

### Added

- The feed, which is also the watcher, with one entry per error and repeats grouped.
- Built-in presets for generic errors, Node, Python, Ruby, Go, Rust, the JVM and test
  runners, each tested against real output.
- Agent choice: the most recent agent in the same tab, `t` to pick another and pins in
  `sightline.toml`.
- A message format that keeps every line from the pane inside a marked data block.
- The fix check, ending `✓ fixed`, `✗ still failing` or `? unverified`.
- `sightline.toml` with live reload, `closing_line` and ignore rules from the feed
  with `n`.
- Release binaries for macOS and Linux, checked against `checksums.txt` on install.
