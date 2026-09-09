# pager

[English](README.md) · [한국어](README.ko.md)

Send messages between coding-agent sessions. Works across tools and across
projects.

It behaves like a pager, not a phone. You send; the other session reads it
**the next time it does anything**. Nothing polls, and nothing interrupts a
session mid-thought.

```
codex session                     claude session
    │                                 │
    │  pager send review-box "…"      │
    ├────────────► ~/.pager/msg.db    │
    │                                 │
    │                        (on its next hook run)
    │                                 │◄── injected into its context
```

## What it's for

You have two agent sessions open: one in the API repo, one in the web repo.
The API one changes a response shape, and the web one needs to know. Today you
read that off one terminal and retype it into the other.

That retyping is what pager automates. It does not try to automate *noticing* —
see [Out of scope](docs/reference.md#out-of-scope).

## Install

```bash
make build     # builds to ~/.local/bin/pager
```

No C toolchain needed: the SQLite driver is pure Go, so it cross-compiles too.
Everything lives in one file, `~/.pager/msg.db`, created as `0600` in a `0700`
directory.

Then register a hook, because **the hook is what delivers**. Sending writes to
the database; only a hook can put text into a session's context.

**Claude Code** — `~/.claude/settings.json`:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "matcher": "", "hooks": [
        { "type": "command", "command": "pager hook", "timeout": 10000 }
      ]}
    ],
    "SessionStart": [
      { "matcher": "", "hooks": [
        { "type": "command", "command": "pager hook", "timeout": 10000 }
      ]}
    ]
  }
}
```

**Codex CLI** — `~/.codex/config.toml`:

```toml
[[hooks.UserPromptSubmit]]
[[hooks.UserPromptSubmit.hooks]]
type = "command"
command = "pager hook"
timeout = 10
```

`UserPromptSubmit` is the one you cannot skip. `SessionStart` is recommended so
a fresh session sees waiting mail right away. Pass no event name — `pager hook`
reads it from the payload, so it cannot be misspelled.

## Send your first message

```bash
pager who                          # who can I reach?
pager send hica "parser is yours"  # send
pager ls                           # what came to me
pager whoami                       # my session, my name
```

Sessions get a four-letter name like `hica` on their first hook run. The name
is pronounceable and means nothing on purpose. Pick your own with
`pager alias review-box` if you prefer.

## When does it arrive?

Sending is always immediate. What varies is when the other side looks.

- **If it's reachable, right away.** `send` knocks on the recipient's host and
  it takes a turn within a few seconds; that turn's hook delivers.
- **Otherwise, on its next activity.** No knock, no loss — the message waits in
  the inbox until a hook runs.

The knock carries no content. It only says "you have mail"; delivery is always
the hook's job, so a failed knock costs you latency and nothing else. `pager
send` tells you which path it took.

## The one caveat

**Delivery is conditional, not guaranteed.** If the recipient's hook never
runs, pager never gets a chance to output anything, and it will not tell you —
every hook path exits quietly rather than blocking a session.

You may also see the same message twice: retry is what makes delivery
survivable, and duplication is its price. [reference.md](docs/reference.md#the-guarantee)
states the exact terms.

## Everyday commands

| Command | What it answers |
| --- | --- |
| `pager who` | Who can I send to, and is anyone actually behind that name? |
| `pager send <name> "…"` | Send. |
| `pager ls` | What came to me. |
| `pager ls --waiting` | What nobody has picked up yet. |
| `pager inbox` | Which inboxes have mail waiting, across all sessions. |
| `pager whoami` | Which session am I, and what is my name? |
| `pager alias <name>` | Choose my own name. |
| `pager claim <name>` | Take over an inbox whose session is gone. |
| `pager export` | Dump the whole store as JSONL. |

## Using it from inside an agent (optional)

The CLI is enough. If you want tool calls instead:

```bash
claude mcp add pager -s user -- pager mcp
codex  mcp add pager       -- pager mcp
```

Three tools: `msg_send` to send, `msg_list` to read your own mail, `msg_roster`
to see who you could send to. An agent learns its own name from the hook, but
learns other names only from mail it receives — hence the roster.

## One safety note

A message body is text another agent wrote, landing in your context. That is a
prompt-injection path, and it cannot be designed away — only reduced. pager
always shows the sender, wraps the body as data rather than instructions, and
caps its length.

The store is local to your user account and never touches the network.

## Documentation

- [reference.md](docs/reference.md) — configuration, retention, rate limits,
  column meanings, troubleshooting
- [hooks.md](docs/hooks.md) — hook contract: events, inputs, outputs, fail-open
- [session-binding.md](docs/session-binding.md) — how a session is identified

Korean versions of all four are alongside them as `*.ko.md`.

## License

MIT — see [LICENSE](LICENSE).
