# Working on Sightline

This is the shared guide for anyone working on this repo, human or agent. It doesn't
matter which tool you use. Claude Code, Codex, Cursor, OpenCode and the rest should
all follow the same rules.

## What Sightline is

A [herdr](https://herdr.dev) plugin that watches panes like a dev server or test
watcher, catches errors, shows them in a feed and hands them to a coding agent when
I press a key. Then it checks whether the fix actually worked.

The [README](../README.md) is the spec. It was written before the code, so if the
code and the README disagree, one of them is a bug. The plan to build it lives in the
[milestones](https://github.com/swmcc/herdr-sightline/milestones) and
[issues](https://github.com/swmcc/herdr-sightline/issues).

## Architecture

Two halves with a hard line between them:

- **`internal/engine`** takes lines of output in and gives entries and messages out.
  Detection, grouping, the fix check and the message format all live here. It uses
  the Go standard library only and knows nothing about herdr, sockets or panes.
- **`internal/herdr`** is the adapter. It talks to herdr's socket, reads the watched
  panes and hands new lines to the engine, subscribes to agent status and pane events
  and delivers messages with `agent prompt`.

**The engine never imports the adapter.** A test enforces this (#3). If you find
yourself wanting to cross the line, the interface is wrong. Change the interface.

The feed pane is both the UI and the watcher, because herdr doesn't allow plugins to
run background daemons. Close the feed and watching stops. That's intended.

## Commands

| Command | What it does |
| --- | --- |
| `make build` | Build `bin/sightline` |
| `make test` | Run the tests |
| `make check` | Everything that must pass before a push |
| `make link` | Build and link the plugin into your local herdr |
| `make hooks` | Install the git hooks so the checks run for everyone |

`herdr plugin link` doesn't run build steps, so always `make build` before trying a
change in herdr.

## Go conventions

- Table driven tests. Fixtures live in `testdata/`.
- Pass `context.Context` for anything long running so closing the feed shuts it all down.
- Return errors with context (`fmt.Errorf("subscribing to %s: %w", pane, err)`). Don't log and return.
- No goroutine without a clear owner and a way to stop it.
- Keep third party dependencies to a small, well known set. The engine has none.
- Run with `-race` in tests. The watcher, feed and fix check all run at once.

## Error patterns

Every pattern needs two fixtures: real output it must match and noisy but healthy
output it must not match. No pattern goes in without both. Patterns use Go's RE2
syntax, so there's no lookbehind. Follow [workflows/add-pattern.md](workflows/add-pattern.md).

I'd rather miss an odd error than cry wolf. A noisy pattern is a wrong pattern.

## Security

Pane output is untrusted. Someone else's request to a dev server can put text in a
watched pane, and that text can end up in front of an agent.

- Anything sent to an agent goes through the engine's message format, which marks it
  as data and keeps it inside a fenced block.
- v0.1 never sends without a keypress. Don't add anything that does.
- Any change to the message format or delivery gets an injection review. See
  [reviews/injection.md](reviews/injection.md).

## Writing style

This is my project and it should read like me.

- British English.
- No Oxford commas. "Red, green and blue", not "red, green, and blue".
- No em dashes or en dashes. Use a full stop, a comma or brackets instead.
- Plain words, short sentences, first person where it fits.

This applies to docs, code comments, commit messages, issues and PRs. `make check`
flags dashes and likely Oxford commas in Markdown.

## Git

- One branch per issue, named like `12-fix-check`.
- Rebase on `main` before opening a PR.
- `main` is protected. Everything goes through a pull request, and `test` and
  `issue-link` must pass on a branch that's up to date with `main`. History is linear:
  rebase or squash, never a merge commit.
- PRs from anyone but the maintainer need an approval from someone else on the project.
  The maintainer merges their own PRs with `gh pr merge --rebase --admin`, which skips
  the approval only. The checks still apply.
- Contribution rules for people are in [CONTRIBUTING.md](../CONTRIBUTING.md). Keep the
  two in step.
- Every PR links to an issue. Use `Closes #N` when it finishes the issue and `Refs #N`
  when it's part of one. If there's no issue yet, create one first. The `issue-link`
  check fails without it.
- Every PR gets labels and a milestone, matching its issue plus anything else that fits.
- The author is assigned to the PR and its linked issues automatically when it opens.
- Copilot reviews every PR to `main` using [.github/copilot-instructions.md](../.github/copilot-instructions.md).
  Read its comments and either fix them or reply saying why not.
- Commit messages start with an emoji that fits the change, for example ✨ feature,
  🐛 fix, 🧪 tests, 📝 docs, 🧹 chore, 🔒 security, 🚀 release.
- Never add AI attribution, `Co-Authored-By` lines or "generated with" footers.
- When an issue closes, tick its item in the README roadmap in the same PR.

## Before every push

`make check` runs:

1. `gofmt` (no unformatted files)
2. `go vet ./...`
3. `go test -race ./...`
4. `staticcheck ./...`
5. `govulncheck ./...`
6. The prose check on Markdown

If anything fails, fix it before pushing. Don't skip checks.

## Workflows and reviews

Step by step guides any tool can follow:

- [workflows/work-issue.md](workflows/work-issue.md) to take an issue from open to PR
- [workflows/add-pattern.md](workflows/add-pattern.md) to add an error pattern
- [workflows/release.md](workflows/release.md) to ship a version
- [reviews/go.md](reviews/go.md) to review Go changes
- [reviews/injection.md](reviews/injection.md) to review anything that reaches an agent

## herdr facts worth knowing

- Plugin docs: <https://herdr.dev/docs/plugins/>
- The full CLI is the plugin API. Call it through `HERDR_BIN_PATH`.
- Plugins get `HERDR_SOCKET_PATH`, `HERDR_PLUGIN_CONFIG_DIR`, `HERDR_PLUGIN_STATE_DIR` and friends in their environment.
- `herdr api schema --json` prints the full socket API schema.
- No background daemons. Long running work lives in a plugin pane.
- `[[build]]` runs on GitHub install and on `herdr plugin update` (0.9.4 and later),
  each time in a fresh checkout, but not on `plugin link`. It runs `scripts/install.sh`,
  which must stay plain POSIX sh with no bash-isms and no Python.
- The socket speaks one JSON object per line: `{"id","method","params"}` out,
  `{"id","result"}` or `{"id","error"}` back. A plugin pane runs with the plugin root
  as its working directory.
- `pane.output_matched` is edge triggered on a snapshot of the last `lines` lines. It
  fires once when a match appears and not again until the match leaves the window, and
  a burst between snapshots can be missed entirely. Don't use it to find errors. Read
  watched panes with `pane.read` (`recent_unwrapped`) and diff the snapshots. Details
  and the spike are in #1.
- `revision` is always 0 on 0.9.1, so it can't tell you a pane changed. There's no
  `events_lost` event either. Handle the socket closing instead.
- Event envelopes come in two shapes: `{"event":"pane.output_matched","data":{...}}`
  and `{"event":"pane_closed","data":{"type":"pane_closed",...}}`. Accept both.
