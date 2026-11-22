# Safety

**Pane output is untrusted.** Anything that prints to a watched pane, including a log
line from a request to your dev server, ends up in front of the agent. Someone could
craft output that reads like instructions.

## What Sightline does about it

- never sends anything without you pressing a key, unless you've turned
  [queue mode](queue-mode.md) on for that pane (it then sends only to an agent it's
  sure of, only when that agent is idle and only as often as the caps allow)
- labels everything it sends as output from another pane, to be treated as data
- keeps everything from the pane, its name included, inside a block where every line
  is marked, so none of it can pass for Sightline's own words
- caps how much it sends
- shows every `@` from the pane as a fullwidth `＠`, so the agent can't take pane text
  for a file mention like `@~/.ssh/id_ed25519` (the message says when it has done this)
- always starts the message with "Sightline", so it can never begin with a `!` or `/`
  that an agent's prompt might run as a command
- shows you exactly what will be sent before you send it (`v` to view), and exactly
  what queue mode sent after it went
- ignores a repo's own `.sightline.toml` until you've read it and trusted it, and asks
  again if a single byte of it changes (see [Project files](project-files.md#trusting-it))

## What that protects against, and what it doesn't

- **It can** keep pane text inside a clearly labelled data block that the pane can't
  close or forge. It strips escape codes and invisible characters. It keeps out text
  hidden in escape codes or drawn over, or flags the error as possibly incomplete
  when it can't be sure. It refuses to send without a keypress unless queue mode
  is on for that pane, and even then it holds back an error it isn't sure of.
- **It can't** stop a model from being persuaded by text it's shown. The label says
  "this is data", but a model can still choose to follow it. Read what you send.
- **It can't** spot text drawn in colours you can't see, like black on black. That
  reaches the message like any other text, so check `v` before you send.
- **It can't** see what herdr's terminal hid. If a pane hides the error coming back
  (in a terminal title, say) then Sightline doesn't see it either. A server that logs
  text someone else chose can still print a line that looks like a recompile or a
  request succeeding, for example when it logs a newline from a request path as
  it is, so the text after it starts a line of its own. A server handling
  requests side by side can also log someone else's request ending where yours
  should. When a request logs more than the context before it fails, its start
  is lost, and a request someone else started in that time can stand in for it.
  A dev server that compiles pages on demand (Next.js) prints a real compile
  line for anyone's request. So `✓ fixed` only ever means the error stopped
  showing.
- **It can't** tell a pane that shows only your own output from one that shows
  other people's. See below.

## Queue mode

**Queue mode lets pane text reach the agent without you reading it first.** On a
pane that shows other people's output, like a dev server taking requests, anyone
who can reach that pane can put text in front of the agent, and nobody will have
read it before it goes. The message still says it's data and the caps limit how
often it can happen, but that's all. Only turn queue mode on for a pane where
you'd be happy for that to happen. The feed says so too before it turns it on.

## Your agent

Sightline can't control the agent that receives the message. If you use Claude Code,
set `CLAUDE_CODE_DISABLE_FILE_MENTIONS=1` in the `env` of your user settings (Claude
Code 2.1.299 or later) and add `Read` deny rules for your secret paths, such as
`Read(~/.ssh/**)`.

Don't watch panes that print secrets. Sightline can't tell a stack trace from an API
key that happens to be on the line above it.

## Reporting a problem

Please don't open a public issue for a security problem. Use
[private vulnerability reporting](https://github.com/swmcc/herdr-sightline/security/advisories/new)
instead. Prompt injection through pane output is the area I care about most.
