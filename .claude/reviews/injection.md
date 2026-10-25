# Injection review

Use this for any change to the message format, delivery or anything else that decides
what reaches an agent. Review it like an attacker who controls the output of a
watched pane.

## Assume the attacker can

- Put any text in a watched pane, including fake instructions and fake system messages
- Print backticks to try to close the fenced block early
- Print very long lines, control characters and ANSI escapes
- Make an error repeat or change slightly to get past grouping

## Check

- [ ] Pane output only ever reaches an agent inside the engine's message format
- [ ] The fence can't be closed early. The fence is longer than any run of backticks in the output, or the output is escaped
- [ ] ANSI and control characters are stripped
- [ ] Length is capped and the cap is visible in the message
- [ ] The message says plainly that the content is data from another pane, not instructions
- [ ] Nothing is sent without a keypress (v0.1), and queue mode follows #21 once it exists
- [ ] `v` in the feed shows exactly the bytes that will be sent
- [ ] There's a test with hostile fixtures for anything new

If any box can't be ticked, the change doesn't merge.
