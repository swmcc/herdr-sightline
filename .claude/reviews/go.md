# Go review

Use this before any PR with Go changes. Report findings with the file and line, and
say how sure you are.

## The boundary

- [ ] Nothing in `internal/engine` imports `internal/herdr` or any third party package
- [ ] New behaviour lives on the right side of the line. Detection, grouping, the fix
      check and message building belong in the engine

## Correctness

- [ ] Every goroutine has an owner and stops when its context is cancelled
- [ ] No data races. Shared state is guarded or owned by one goroutine
- [ ] Errors are wrapped with context and handled once, not logged and returned
- [ ] Reconnects, `events_lost` and closed panes are handled, not ignored
- [ ] Timers and tickers are stopped

## Tests

- [ ] New behaviour has table driven tests
- [ ] Error patterns have both fixtures (see [workflows/add-pattern.md](../workflows/add-pattern.md))
- [ ] Tests pass with `-race`

## Readability

- [ ] Names say what things are. No `mgr`, `util` or `helper`
- [ ] Comments explain why, not what
- [ ] Comments and docs follow the writing style in [AGENTS.md](../AGENTS.md)
