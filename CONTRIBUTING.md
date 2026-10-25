# Contributing to Sightline

Thanks for wanting to help. Sightline is a small project and every bit of help shows.

The [README](README.md) is the spec. It was written before the code, so if the two
disagree, one of them is a bug and I'd like to hear about it. The plan lives in the
[milestones](https://github.com/swmcc/herdr-sightline/milestones).

## The short version

1. Everything starts with an issue. No exceptions, not even for typos.
2. Get assigned to the issue before you start, so two people don't do the same work.
3. One branch and one pull request per issue.
4. `make check` passes before you push.
5. The PR says `Closes #N`, and its checks pass.
6. Someone other than the author approves it. Then it's rebased onto `main`.

## Start with an issue

If there's an issue for what you want to do, comment on it to say you'd like to take
it and I'll assign you. If there isn't one, [open one](https://github.com/swmcc/herdr-sightline/issues/new/choose)
first and say what you're planning. For anything bigger than a small fix, wait until
we've agreed the approach. It saves you writing code that doesn't fit.

Issues labelled [🌱 good first issue](https://github.com/swmcc/herdr-sightline/issues?q=is%3Aopen+label%3A%22%F0%9F%8C%B1+good+first+issue%22)
or [🙋 help wanted](https://github.com/swmcc/herdr-sightline/issues?q=is%3Aopen+label%3A%22%F0%9F%99%8B+help+wanted%22)
are good places to start. Anything labelled `💭 needs design` needs a decision in the
thread before any code.

## Getting set up

You'll need [Go](https://go.dev/dl/) (the version in `go.mod` or newer) and, to try
your change for real, [herdr](https://herdr.dev) 0.9.1 or newer on macOS or Linux.

```sh
git clone https://github.com/swmcc/herdr-sightline
cd herdr-sightline
make setup   # installs staticcheck and govulncheck, and the git hooks
make link    # builds bin/sightline and links the plugin into your herdr
```

`herdr plugin link` doesn't run build steps, so run `make link` again after each change.
`make unlink` takes it back out.

## Making a change

- **Branch from `main`** and name it after the issue, like `12-fix-check`.
- **Keep the engine free of herdr.** `internal/engine` never imports `internal/herdr`.
  A test enforces it. That boundary is what lets Sightline outlive any one tool.
- **Write tests.** Table driven, with fixtures in `testdata/`.
- **Error patterns need two fixtures:** real output the pattern must match and healthy
  output it must not. See [the pattern guide](.claude/workflows/add-pattern.md).
- **Pane output is untrusted.** Anything that changes what reaches an agent gets the
  [injection review](.claude/reviews/injection.md).
- **Update the README** if behaviour changes, and tick the issue's roadmap item.

### Writing style

Docs, comments, commits and PRs are in British English, with no Oxford commas and no
em dashes. `make check` flags them in Markdown.

### Commits

Start each commit message with an emoji that fits:

| Emoji | For |
| --- | --- |
| ✨ | a feature |
| 🐛 | a fix |
| 🧪 | tests |
| 📝 | docs |
| 🧹 | housekeeping |
| 🔒 | security |
| 👷 | CI |
| 🚀 | a release |

Please leave out AI attribution and `Co-Authored-By` footers.

## Pull requests

Open the PR against `main`. The template asks for the issue, what changed and how you
checked it. Then these run on their own:

| What | When | What it does |
| --- | --- | --- |
| `test` | every PR and every push to `main` | prose check, `go vet` and `go test -race` |
| `issue-link` | every PR | fails unless the description links an issue with `Closes #N` or `Refs #N` |
| `assign` | when a PR opens | assigns you to the PR and the issues it links |
| Copilot review | every PR and every new push | reviews against the [shared guide](.claude/AGENTS.md) and the review checklists |

Read Copilot's comments and either fix them or reply saying why not.

### Before it can merge

`main` is protected, for everyone including me:

- Changes reach it through pull requests only
- `test` and `issue-link` must pass, on a branch that's up to date with `main`
- History stays linear. PRs are rebased or squashed, never merged with a merge commit
- No force pushes to `main` and no deleting it

**If you're not the maintainer, someone else on the project has to approve your PR**
and every conversation on it has to be resolved. A new push after approval needs
approving again. Copilot's review doesn't count as an approval.

If `main` moves while your PR is open, rebase onto it and push with
`git push --force-with-lease`.

## Working with coding agents

You're welcome to use Claude Code, Codex, Copilot, Cursor or anything else. The rules
are the same either way, and you're responsible for every line your agent writes.

The repo is set up so agents pick up the rules on their own:

| File | Who reads it |
| --- | --- |
| [`AGENTS.md`](AGENTS.md) | Codex, Cursor, OpenCode and most other agents. It points to the full guide |
| [`.claude/AGENTS.md`](.claude/AGENTS.md) | The full guide that every tool shares |
| [`.claude/CLAUDE.md`](.claude/CLAUDE.md) | Claude Code, which pulls in the full guide |
| [`.github/copilot-instructions.md`](.github/copilot-instructions.md) | Copilot, for reviews and suggestions |

Step by step guides any agent can follow:

- [Work an issue](.claude/workflows/work-issue.md) from open to pull request
- [Add an error pattern](.claude/workflows/add-pattern.md)
- [Release a version](.claude/workflows/release.md)
- [Go review](.claude/reviews/go.md) and [injection review](.claude/reviews/injection.md) checklists

If you use Claude Code, these are also skills and agents: `/work-issue <n>`,
`/add-pattern` and `/release`, plus the `go-reviewer` and `injection-reviewer` agents.
The project settings format Go files and check the prose straight after each edit.

The git hooks from `make setup` run the same checks whichever tool you use, so an agent
can't push something that `make check` would reject.

## Security

Please don't open a public issue for a security problem. Use
[private vulnerability reporting](https://github.com/swmcc/herdr-sightline/security/advisories/new)
instead. Prompt injection through pane output is the area I care about most.

## Be kind

Be patient and assume good intent, with me and with each other.

## Licence

Sightline is [MIT licensed](LICENSE). By contributing, you agree your contributions are
licensed the same way.
