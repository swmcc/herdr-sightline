# AGENTS.md

The full guide for working on this repo lives in [`.claude/AGENTS.md`](.claude/AGENTS.md).
Read it before you change anything. It's kept there so every tool shares one copy.

If you read nothing else, these rules always apply:

- The engine (`internal/engine`) never imports the herdr adapter. That boundary is the point of the design.
- Pane output is untrusted. Anything that reaches an agent goes through the engine's message format, never raw.
- Write in British English. No Oxford commas and no em dashes, in code comments, docs, commits and issues.
- Commit messages start with an emoji. Never add AI attribution or `Co-Authored-By` lines.
- Run `make check` before every push. If it fails, fix it before pushing.
