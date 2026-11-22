# Configuration

Sightline reads `sightline.toml` from its config directory
(`herdr plugin config-dir swmcc.sightline`). Everything is optional.

```toml
# How long to watch after a fix before calling it.
settle_seconds = 30

# Lines of output sent before the error, and at most this many after it.
# The lines after stop early once the stack trace ends. It can be 0 to 200.
context_lines = 20

# Built-in pattern sets. All are on by default.
presets = ["generic", "node", "python", "ruby", "go", "rust", "jvm", "php", "elixir", "dotnet", "swift", "tests"]

# The last line of the message to the agent. One line, at most 200 characters.
closing_line = "Please investigate and fix the cause."

# Your own patterns.
[[pattern]]
name = "my-framework"
regex = "^FATAL: .+"

# Never alert on these, even if a pattern matches.
[[ignore]]
regex = "DeprecationWarning"

# Send a pane's errors to a particular agent, and have the feed remind you
# about queue mode for it.
[[watch]]
pane_title = "web"
agent = "claude"
mode = "queue"
```

Patterns use Go's [RE2 syntax](https://github.com/google/re2/wiki/Syntax). There's no
lookbehind or backreferences, but matching always runs in linear time, which matters
when the text being matched is untrusted.

For `[[watch]]`, see [Which agent gets the error](how-it-works.md#which-agent-gets-the-error)
and [Queue mode](queue-mode.md). A repo can carry its own patterns and ignore rules
too. See [Project files](project-files.md).

## Changes apply as you save

There's no need to reopen the feed and nothing in it is lost. Patterns and ignore
rules apply to the next line. A new ignore rule also dismisses any new entries it
matches, but never one you've sent. A new `settle_seconds` or `context_lines` applies
to the next error or fix check, not one already under way. A new `closing_line`
applies to the next message. If the file has a mistake in it, the feed shows the
error and carries on with the last good version.

## The closing line

`closing_line` is an instruction to the agent, so Sightline refuses one that has a
line break, a control or invisible character, any space but a single plain one, or
more than 200 characters, rather than quietly changing it. Backticks are turned into
apostrophes and an empty one means the default.

## Ignore rules from the feed

Press `n` on any entry to add an ignore rule for it without editing the file. The
feed suggests a regex from the error line, with the parts that change between runs
(numbers, ids, hex, quoted values and paths) made general. Change it as you like.
It shows whether the rule compiles and still matches the line, and won't save one
that matches an empty line, ordinary output or other errors that start the same
way. If making the parts general would do that, it suggests the line exactly as
printed instead. Nothing is written until you press `enter`. The rule goes at the
end of `sightline.toml` as a new `[[ignore]]` table and the rest of the file is left
exactly as it was. It applies when the feed reloads the file a moment later. Press
`u` within 30 seconds to take it back out.
