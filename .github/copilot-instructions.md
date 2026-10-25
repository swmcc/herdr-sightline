# Copilot instructions

The full guide for this repo is [`.claude/AGENTS.md`](../.claude/AGENTS.md). It's shared
by every tool so there's one set of rules. Follow it.

## When reviewing a pull request

Review against these, in this order:

1. [`.claude/reviews/injection.md`](../.claude/reviews/injection.md) for anything that
   changes what reaches an agent: the message format, delivery or queue mode. Treat
   pane output as hostile. This matters most.
2. [`.claude/reviews/go.md`](../.claude/reviews/go.md) for all Go changes. The engine
   (`internal/engine`) must never import the herdr adapter (`internal/herdr`).
3. Error patterns need both fixtures: output they must match and output they must not.

Report real problems with the file, the line and what would go wrong. Skip anything
`gofmt`, `go vet` or `staticcheck` already catch.

## Writing style

Docs, comments and messages are in British English with no Oxford commas and no em
dashes. Flag any that slip in.
