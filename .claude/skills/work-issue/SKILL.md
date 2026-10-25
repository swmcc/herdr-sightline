---
name: work-issue
description: Take a herdr-sightline GitHub issue from open to a pull request. Use when asked to work on, pick up or implement an issue by number.
---

Follow `.claude/workflows/work-issue.md` exactly, for the issue number given in the
arguments. If no number was given, list open issues in the current milestone with
`gh issue list --milestone "<milestone>"` and ask which one.

Before opening the PR, run the `go-reviewer` agent. If the change touches the message
format or delivery, run the `injection-reviewer` agent as well.
