# The session binding contract

[English](session-binding.md) · [한국어](session-binding.ko.md)

> All of hop safety rests on this document. Changing what is decided here
> changes the confirmation conditions in `internal/sessionref` and
> `internal/deliver` along with it.
>
> See also: [hooks.md](hooks.md), [reference.md](reference.md)

## Why a workaround is needed at all

**The MCP protocol does not give the caller's `session_id`.** Five sessions
means five MCP server processes (one instance per session), and there is no
session id in the environment either. So the MCP path cannot know its own
session.

The CLI has the same problem. **`pager send` is the primary send path**, and a
CLI invoked by an agent's Bash tool does not know its own session id either.
Solving only MCP would leave every `pager send` `unattributed` and blocked by
pager's own policy.

The only party that knows the session context is **the hook**. Hooks receive
`session_id` on stdin. So there is one contract — **the hook records, everyone
else looks it up.** The CLI and MCP share one resolver
(`internal/sessionref`).

## A proven precedent

This structure was not invented here. A sibling tool that hit the same problem
first — MCP not telling you the caller's session — arrived at the same answer
in production use: the hook writes an active-session pointer keyed by host
process identity, and the CLI and MCP read it.

Where pager differs is **one thing: where that pointer lives** (see "Where the
active binding lives" below).

## Host process identity

The hook and the CLI/MCP are different processes. For the two to point at the
same session, they must **independently name the same host with the same
value.**

```
Instance{ Pid int, Start int64 }
```

`Start` is a platform-relative process start token — on darwin, `P_starttime`
(ms) from the `kern.proc.pid` sysctl; on linux, field 22 (starttime jiffies) of
`/proc/<pid>/stat`. The unit is meaningless. **All that matters is the
stability of two processes on the same machine reading the same value.**

Detection walks **up the ancestor chain**:

```
pid = os.Getppid()
up to 16 levels up:
    procInfo(pid) → (ppid, start, cmd)
    if the basename of cmd's comm or argv0 matches the client name → Instance{pid, start}
    otherwise pid = ppid
not found within 16 levels → detection failed
```

- The MCP server is usually a **direct child** of the host.
- A CLI run by the agent's Bash tool is a **descendant** one or two shells down.
- Hooks are descendants in the same way.

All three paths reach the same ancestor, so all three get the same `Instance`.

**The full argv is not used for matching.** A hook command's arguments can
contain a path like `~/.codex`, so looking at the whole argv would mistake a
non-codex process for a codex host. Only `comm` and `argv0` are examined.

The host tool label (`claude`/`codex`) checks the `PAGER_CLIENT` environment
variable first and falls back to the parent process name. The `PAGER_` prefix
is not the `CONTEXT_` prefix, so it is not subject to Claude Code's `CONTEXT_*`
environment scrubbing.

## Resolution tiers — shared by CLI and MCP

The single entry point in `internal/sessionref` tries these in order. **Every
send path uses the same tiers** — CLI or MCP, no exceptions.

| Tier | Method | Where it applies |
| --- | --- | --- |
| 1 | `--session <id>` argument | Hooks (their own stdin has the session_id), scripts |
| 2 | `PAGER_SESSION` environment variable | Planted by the hook, inherited by every child process of that session |
| 3 | Host detection → look up the active session the hook recorded | MCP servers, a CLI the agent ran through Bash |
| 4 | Failure → `unattributed` | **Refused** by default (without `--human`) |

## There are two addressing layers — the automatic name is not a third

| Layer | Value | Nature |
| --- | --- | --- |
| Session | `session_id` | Given by the host. Immutable, not meant for humans |
| Name | `aliases.alias` | The logical address. **One attaches automatically**, and a human can add more |

The automatic name is not a separate layer but **the default value of the name
layer.** The reason lies in the delivery path: `Candidates` and `revalidate`
find the recipient with `messages JOIN aliases ON alias`
(`internal/deliver/lease.go`), so an address that is not an alias row cannot
receive at all. Keeping the automatic name in a `sessions` column would require
a UNION in the claim-revalidation SQL, and that touches the most dangerous of
the contracts this document pins down.

Issuance rules:

- **When** — every time a session without a name runs a hook, and on
  `pager attach`. `SessionStart` is only a recommended grade, so it alone is not
  enough.
- **No name is issued when the workspace is unknown.** An alias row copies and
  pins the `root` and `tool` at insertion time and nobody updates it afterwards.
  Attaching one while detection has failed makes that name disappear from
  orphan discovery, and claim will not transfer it either. The verdict comes
  from **the stored `sessions` row**, not from the value the caller holds —
  since `RecordSession` does not erase existing values with blanks, a session an
  earlier `attach` filled in stays eligible even if this hook's detection fails.
- **Concurrency** — issuance is a single INSERT with
  `NOT EXISTS (an alias for this session)` in its condition. Splitting the check
  from the insert lets two concurrent hooks each decide they are first, giving
  one session two names.

### Which name is *the* name for that session

`ORDER BY updated_at DESC, alias` — **the name decided later wins.** The
automatic name attaches when the session starts, so a name a human chose later
always comes after it.

That is why `SetAlias` and `ClaimAlias` do not use the clock as-is but a value
**strictly greater than the target session's existing names**:
`max(now, (SELECT max(updated_at) ... WHERE session_id = target) + 1)`. The
clock has millisecond resolution, so two writes can land on the same value, and
then the tiebreak falls to alphabetical order and the automatic name wins.

### Ambiguity is per session, not per name — but only in tiers that name a session

Letting one session hold several names changed how targets resolve.

| Tier | What it names | On multiple rows |
| --- | --- | --- |
| `alias` | a name | impossible (primary key) |
| `session` | **a session** | same session, so fold to a representative name |
| `pm_ref` | **a session** | fold if all the same session; `AmbiguousError` if different sessions are mixed in |
| `live substring` | a name pattern among live sessions | show candidates and fail — this is a human's choice |
| `substring` | a name pattern | show candidates and fail — there is no way to know which name was meant |

The folding condition includes "`session_id` is not empty". Orphan aliases have
no session, and two of them compare equal on that value (the empty string) while
actually being different inboxes.

### Why substring matching has two layers

**Names are never reclaimed.** Every session that ever ran leaves a name behind,
and that name stays after the session ends. With only one substring layer,
**the set a human picks from and the set a reference resolves against drift
apart** — `pager who` shows only live ones while resolution contends with dead
ones too. An abbreviation that was unique when learned then starts colliding
with the name of a session that ended weeks ago, and that denominator only
grows with time.

`live substring` applies a staleness bound (`heartbeat_at >= now - staleAfter`)
so it sees only live names. Several matches here **fail rather than falling
through to the next tier** — overlap among live names is a real question for a
human to answer, and widening the scope would only add candidates nobody is
waiting behind.

Because the broad `substring` tier remains behind it, **a name whose session has
left is still callable.** Handover starts with mail piling up in that inbox, so
it must not become unreachable. What changed is priority, not reachability.

This affects only how patterns match. **Calling a name exactly is unchanged** —
the `alias` tier is first, and an exact name is precisely what the orphan notice
tells the human.

## Where the active binding lives

**In `sessions` table columns.** There is no separate pointer file.

The precedent kept a separate pointer file because **its hook and MCP server
used different stores** — the server wrote to its own database while session
context existed only in the hook, so a file was needed to join them. pager has
no such premise: the hook, the CLI and MCP **all write the same
`~/.pager/msg.db`.** A pointer file would add a second store, plus its TTL and
orphan cleanup, as entirely new surface.

```sql
-- host columns on the sessions table (internal/store schema)
host_client TEXT,     -- 'claude' | 'codex'
host_pid    INTEGER,
host_start  INTEGER   -- platform-relative start token
```

`heartbeat_at` plays exactly the TTL role the pointer-file approach needed.
Freshness collapses onto one axis, so there is no atomic rename and no separate
expiry file.

### What the hook writes — last writer wins

Running `/clear` or a resume on the same host **keeps the host pid and changes
the session_id.** So recording has to be a **key transfer**, not a plain
UPDATE. Two statements in one transaction:

```sql
-- 1) detach this host key from whichever other session held it
UPDATE sessions SET host_pid = NULL, host_start = NULL
 WHERE host_client = :c AND host_pid = :pid AND host_start = :start
   AND session_id <> :sid;

-- 2) attach it to this session
UPDATE sessions
   SET host_client = :c, host_pid = :pid, host_start = :start, heartbeat_at = :now
 WHERE session_id = :sid;
```

**Invariant: at most one row holds any given `(host_client, host_pid,
host_start)`.** Statement 1 guarantees it, and the tier-3 lookup rests on it.
If the invariant breaks, the lookup meets multiple rows and cannot tell which
session it is, so **it is pinned by tests.**

### The tier-3 lookup

```sql
SELECT session_id FROM sessions
 WHERE host_client = :c AND host_pid = :pid AND host_start = :start
   AND heartbeat_at >= :cutoff;
```

Zero rows falls through to tier 4 (`unattributed`). Thanks to the invariant
above it cannot exceed one row.

## Why pid reuse does not cause misdelivery

The precedent blocks pid reuse with a separate liveness check. pager does
**not need one** — `host_start` already does that job.

Suppose the host died and an unrelated process inherited its pid. Calling
`pager send` from that process builds the lookup key from
`(client, pid, start)` **as detected by the caller itself.**

- If the new process is not a claude/codex host → ancestor matching fails →
  detection fails → tier 4
- If it is a claude/codex host → its start time differs, so `start` differs →
  zero rows → tier 4

That is, **the lookup key is only ever built from a living caller**, so there is
no way to reach a dead host's stale row. What the `heartbeat_at` cutoff guards
is something else — **the host being alive while the hook has not run for a
while** (hooks unregistered or broken). Then the record is considered stale and
refused.

## Restart and multi-session isolation

| Situation | Result |
| --- | --- |
| Host restart | pid or start differs, so it is a **new key**. The old row becomes residue matching no caller, and its staleness makes its alias claimable |
| `/clear` or resume on the same host | Same pid, new session_id → key transfer, so **the new session inherits**. The old session's row has NULL host_\* and is unreachable via tier 3 |
| claude and codex at once in the same project | `host_client` differs, so the keys differ → no interference |
| Two instances of the same tool (different windows) | pids differ, so the keys differ → no interference |
| One session across several workspaces | Irrelevant, since session_id is the axis. Alias isolation is guaranteed separately by `root + tool` |

## fail-closed

Failed detection and a zero-row lookup fall **toward refusal**. In the
`unattributed` state, a send without `--human` exits non-zero and is not
inserted into the queue.

The cost of this choice is clear: if the host process structure changes,
**every CLI send blocks immediately.** That is better than silently
misdelivering, and being detected immediately is why this is the right
direction.

**The hook path is the exception.** Hooks must be fail-open on every path
(exit 0, no stdout), because a failing hook blocks the session itself. Hooks
know their own session from tier 1 (`--session`), so they do not depend on
tier 3 of this contract.

## `--human` is not a check

`--human` is **an operator assertion by the caller**, not a securely verified
human origin. An automated agent can attach it, and doing so breaks the hop
chain. So the flag is not a line of defence — **the last line of defence is the
breaker.**

## Pinned dependencies

| Item | Version | Rationale |
| --- | --- | --- |
| SQLite driver | `modernc.org/sqlite v1.50.1` | Pure Go → no CGO, cross-compiles |
| MCP SDK | `github.com/mark3labs/mcp-go v0.52.0` | Uses only the minimum surface a stdio server needs |
| syscall wrapper | `golang.org/x/sys v0.44.0` | darwin `SysctlKinfoProc` / linux `/proc` parsing |

The three are imported by `internal/store`, `internal/mcpsrv`, and
`internal/sessionref` / `internal/wake` respectively. The table above is the
basis for deciding on a version bump — the syscall wrapper especially, since its
darwin and linux paths diverge, so a bump needs the `internal/sessionref` tests
run on both platforms.

## Conditions that break this contract

- **The host executable is renamed** — if `comm`/`argv0` no longer starts with
  `claude`/`codex`, detection fails. `PAGER_CLIENT` can override the label, but
  it cannot override a failed ancestor match.
- **The host stops running the CLI as a descendant** — a broken parent chain
  disables tier 3. Then the session must be named explicitly via tier 1
  (`--session`) or tier 2 (`PAGER_SESSION`).
- **A wrapper chain deeper than 16 levels** — detection fails.
- **Anything other than darwin/linux** — `procInfo` is unsupported, so tier 3
  always fails. Only tiers 1 and 2 work.

All of these fail closed, and the first two surface immediately by blocking CLI
sends outright.
