# Changelog

Every release has an entry here, newest first, written in the commit that
bumps `Version` in `internal/version/version.go`.
`TestChangelogHasCurrentVersion` fails while that version has no
`## <version>` heading at the top, so a bump without patch notes fails
`go test ./...` and therefore `make reinstall`.

Each entry says what changed for someone running pager: commands, MCP tools,
delivery and wake behaviour, environment variables, the store schema, and the
relay protocol. Its Upgrading note says whether an older binary can still open
the store, and what to restart.

Versions are not git tags. `go install …@latest` keeps installing the newest
commit on `main`; `pager version` names both the release and the build.

Entries 0.1.0 to 0.5.0 were written on 2026-11-04 from the commit history.
pager had no versions then, so their boundaries were drawn afterwards along
the history's natural stages. The MCP server reported `0.1.0` throughout.

## 0.7.0 — 2026-11-07

### Added

- A Claude Code session is now named after its pager name. On
  `UserPromptSubmit`, `pager hook` sets `hookSpecificOutput.sessionTitle` when
  the title Claude Code reports differs, and Claude Code applies it as a
  rename: the title, the transcript's agent name and the live registry name
  that `ListAgents` and `SendMessage` address all become the pager name. A new
  session takes it on its first prompt, a resume keeps it, and `/rename`,
  which runs no hook, is undone on the next prompt. It is sent only when
  `AI_AGENT` names Claude Code (2.1.120 and later) and the session's stored
  tool is `claude`; SessionStart, Stop, Codex and every other host keep their
  output unchanged. If another live Claude session on the host holds the name,
  Claude Code suffixes it for the life of that process; a resume or restart
  puts it back. See [docs/hooks.md](docs/hooks.md#session-title-claude-code).
  (9cc3fef)

### Upgrading

No store or relay change: schema 5 and relay protocol 2, as in 0.6.x, so 0.6.x
and 0.7.0 binaries can share a store. The hook binary is run per event, so the
new behaviour starts at the next prompt after reinstalling; nothing needs a
restart. Claude Code older than 2.1.120 sets no `AI_AGENT` and is unaffected.

## 0.6.1 — 2026-11-07

### Fixed

- A send right after a Codex restart now wakes the session. The restarted Codex
  runs its sessions under a new app-server process, and the session record
  follows only at the session's first hook, so until then pager judged the
  holder gone and skipped wake, leaving the mail unread. A holder whose
  recorded host is gone is now poked too: a poke that lands is reported as
  `poked`, a refusal keeps its diagnosis without promising a next activity,
  and anything else prints the `session is gone` note as before. (5909bd9)

### Upgrading

No store or relay change: schema 5 and relay protocol 2, as in 0.6.0, so 0.6.0
and 0.6.1 binaries can share a store. Restart running `pager mcp` processes
after reinstalling to get the new send behaviour from MCP `msg_send`.

## 0.6.0 — 2026-11-04

### Added

- `pager version` (also `--version`) prints the release and the build it came
  from: the commit of a checkout build, or the module version `go install`
  resolved. The MCP server and the Codex app-server client report the same
  release instead of a fixed `0.1.0`.
- `pager relay wait --peer <p> [--timeout d]` is a long poll that returns when
  mail is waiting for the peer, so a transport that reaches the other store by
  running commands there no longer has to run `relay out` every interval.
  (65515fe)
- `msg_send` takes optional `task`, `kind` (`update`, `result`, `failure`,
  `release`), `activity` (`working`, `idle`, `unknown`), `progress` and
  `event_id`. Retrying an `event_id` with the same content keeps the original
  message. (6fedd3e)

### Changed

- Message quotas and the three-hop cutoff are gone. Hop and cause ids remain
  attribution data. Automatic waking pauses only when both local peers report
  `activity=idle` with a fresh signal and the same two-way bodies and progress
  stay unchanged for `PAGER_LOOP_IDLE` (default 2m). `PAGER_LOOP_SIGNAL_TTL`
  (default 2m) bounds the age of an idle signal. Imports from another store
  have no hourly cap. Results, failures and releases always wake. (6fedd3e)
- Codex wakes coalesce: at most one pending pager notification per thread,
  merged through Codex's native queue APIs. Reading the mail removes queued
  notifications that are now obsolete. Unsupported responses defer the wake
  and keep the mail. (e3f2312)

### Upgrading

The store moves to schema 5 (event columns and the loop guard table); a 0.5.0
binary refuses to open it, so upgrade every pager on the machine together.
The relay protocol moves to version 2. Extended events and long chains need a
0.6.0 peer. Restart running `pager mcp` processes after reinstalling: hooks
pick up the new binary, an MCP server keeps its old code.

## 0.5.0 — 2026-10-28

### Added

- Relaying between stores over any transport. `pager peer add|ls|rm|rebind`
  registers the other stores. `name@peer` addresses a session there. `pager relay out`,
  `relay in` and `relay ack` move records as ASCII JSON Lines with exit
  codes a transport can act on (3 retry, 2 look at it). `relay id` and
  `relay new-id` name a store and give a restored one a new identity.
  (fd5f29a..16cf6e2)
- Relayed mail carries a provenance line ("relayed from peer mac — reply to
  sape@mac") in hook delivery, `msg_list` and `pager ls`, and stays until it
  has left. `pager who` and `msg_roster` name the peers and when each link
  last worked. (30768ec, 16cf6e2)

### Changed

- The CLI's `send` and MCP `msg_send` share one send path. A stored send is
  reported even when the follow-up lookup fails. (175bdf9, 2929e89)
- A busy Claude session is poked without interrupting its turn. (35da473)
- Rejections offer each live session once, and remote-send limits are worded
  alike. (c76752c, c39ed39)

### Fixed

- Every line break inside a quoted body stays quoted, including carriage
  returns and Unicode separators. (806b7df)
- Relay bounds found by review: label and name sizes, host name length, the
  store id named in a wrong-store refusal. (e7f27a6, 4abd6ed, 4290c15)
- Windows PowerShell 5.1's doubled byte-order marks no longer make every
  relay round malformed. (ad6a9f4)
- Codex callers resolve to their own thread, and a stale queued wake prompt
  is blocked when a successful inbox check finds nothing waiting. (4b46b64)

### Upgrading

The store moves to schema 4 (relay columns, peers and the store id); older
binaries refuse it. Relay protocol version 1.

## 0.4.0 — 2026-10-12

### Added

- Windows: host-process detection, so automatic names and session resolution
  work there, and Claude sessions are woken through their named pipe, after
  checking the pipe's server pid. (8ab19ee, 2ba334e, a37ca10)

### Changed

- Published: the module path is `github.com/iMUngHee/pager`, under the MIT
  license. The README leads with English and the reference moved to `docs/`.
  (e765277, fdacf5a, 05bd73b)
- A host whose start token cannot be read is refused, not matched with a token
  of 0. (84eb440)

### Upgrading

No schema change (schema 3). Installs from the old `github.com/unghee/pager`
module path must reinstall from the new one.

## 0.3.0 — 2026-09-07

### Added

- Wake: after a send, pager pokes a recipient with a live session over the
  surface its host offers, so mail can arrive now instead of on the next
  activity. `send` says which happened. (f97f1a4, e7646ee, c9565e8)
- `pager inbox` lists every inbox with mail waiting in one call. (61f3e92)
- `msg_roster` and `pager who` say who can be paged, whether each host is
  still there, and what each session is working on. (d12ce01, a9eb978)

### Changed

- pager's own poke no longer ends the recipient's exchange as if a user had
  typed it. The poke names the sender's tool, so a Codex sender does not read
  as Claude. (6878ad0, da464cd)
- A blocked sender is told what to do about it, and a send to a name nobody is
  behind says so. (f7c4469, 5440fea)

### Upgrading

The store moves to schema 3 (`sessions.purpose`); older binaries refuse it.

## 0.2.0 — 2026-08-21

### Added

- Every session gets a name it can be called by. `pager who`, `whoami` and
  `attach` show it. A short reference finds a live session first.
  (fb53d50, 1abc98c, 29424f5)
- `make reinstall` tests, smoke-checks that the new binary opens its store,
  swaps it in atomically and keeps the last three binaries; `make rollback`
  steps back through them. (667ea5c, c1624fa, 162812b)
- Prune writes the rows it removes to an append-only JSONL archive first.
  (dd38573)
- `pager ls --waiting` and `msg_list` `waiting` return only unread mail; a
  poll records what it read (`listed_at`), and exports keep it.
  (34cc487, 5cb71eb, 2ddce6a, f3fc0c5)

### Fixed

- Flags written after a positional argument are parsed. (17f7085)
- A cancelled write no longer poisons its connection, and two prunes never
  write the archive at once. (28124f4, 7d538d8)
- Migrations replay, so an existing database upgrades instead of refusing to
  open. (0f15c2b)

### Upgrading

The store moves to schema 2 (`messages.listed_at`); older binaries refuse it.

## 0.1.0 — 2026-08-11

First working version.

- One SQLite store (pure Go, no CGO) with retention prune, and a four-tier
  session resolver shared by the CLI and MCP.
- Aliases with race-safe claiming, causal chains bounded by an immutable
  hop, epoch and sequence, and delivery of claimed batches under a lease, a
  budget and a window.
- One hook adapter for Claude Code and Codex, event names in any case.
- CLI `send`, `attach`, `alias`, `claim`, `ls`, `whoami`, `prune`, and MCP
  `msg_send` and `msg_list`. An agent's message is never labelled human.

### Upgrading

Schema 1.
