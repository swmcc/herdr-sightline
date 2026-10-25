@AGENTS.md

## Claude Code specifics

- Workflows are available as skills: `/work-issue`, `/add-pattern` and `/release`.
  They follow the shared guides in `.claude/workflows/`.
- Use the `go-reviewer` agent before opening a PR with Go changes and the
  `injection-reviewer` agent for anything touching the message format or delivery.
- Hooks run `gofmt` after Go edits and the prose check after Markdown edits. If the
  prose check complains, fix the wording rather than ignoring it.
