# The hook contract — event / input / output matrix

[English](hooks.md) · [한국어](hooks.ko.md)

> Delivery is entirely the hook's job. MCP is a pull model, so a server cannot
> wake the recipient's turn, and injection is only possible from a hook.
>
> See also: [session-binding.md](session-binding.md)

## Why hooks

pager's guarantee is "you see it the next time you do anything". A hook is the
only place that "anything" can be observed. If the recipient session never runs
a hook, **the number of output attempts is zero** — this is what makes the
guarantee conditional.

## Both tools use the same contract

Claude Code and Codex CLI both take JSON on stdin, and both fold
`hookSpecificOutput.additionalContext` from stdout into the next turn's
context. Because the two hosts share one contract, a single hook attaches to
both as-is and cross-tool plumbing needs no translation layer.

```json
{
  "hookSpecificOutput": {
    "hookEventName": "UserPromptSubmit",
    "additionalContext": "pager: 1 message waiting for you.\n…"
  }
}
```

## Event matrix

| Event | Record session | Reset causality | Deliver messages | Orphan-alias hint |
| --- | --- | --- | --- | --- |
| `UserPromptSubmit` | ✅ | **only with a prompt body, and only if it is not pager's own poke** | ✅ | ✅ (once per session) |
| `SessionStart` | ✅ | ❌ | ✅ | ✅ (once per session) |
| `Stop` | ✅ | ❌ | ✅ | ❌ |
| `SubagentStop` | ✅ | ❌ | ✅ | ❌ |

**Why the Stop family emits no orphan hint** — output from a Stop hook keeps
the conversation going. Emitting a hint with no message to deliver wakes the
session up to read "your inbox is offline" and nothing to act on: a turn that
spends budget and delivers nothing. **Delivery itself still happens on Stop** —
only the hint is suppressed.

**How a causality reset is decided** — all three conditions must hold. The
event is `UserPromptSubmit`, the payload's `prompt` is not blank, and that
`prompt` does not contain `deliver.PokeSentinel`. A host that resumes itself
after a Stop injection has no prompt body of its own, so it does not reset, and
sends going out then still count as `caused`.

**Why the third condition exists** — a poke from wake arrives through the
host's own message-injection path, so at the hook boundary it is
**structurally indistinguishable from a human pressing enter.** Under the older
rule, a single poke raised the recipient's `causal_epoch` and cleared
`last_inbound_id`. Those are measured values — 4→5, 51→NULL.

Synthetic wakes must not remotely reset the receiver's causal epoch or make
an inherited reply look operator-initiated. Hop remains attribution data; the
old hourly send budgets and three-hop cutoff have been removed. The epoch also
resets loop suspicion when a genuine user prompt starts a new task.

The sentinel keeps automatic waking from masquerading as human progress. It
is not an authentication token or a traffic-budget bypass. The signal-based
wake guard requires explicit fresh idle state from both peers plus a repeated,
unchanged exchange; missing activity is unknown and never a reason to pause.

## Codex startup deliveries

Codex runs SessionStart before UserPromptSubmit in a newly loaded turn. When
SessionStart delivers mail, pager records one startup-delivery credit in an
empty sidecar beside the selected database (`<db>.startup-<session hash>`).
The following prompt atomically takes it, by renaming it to a name only that
hook knows and then removing that (on macOS, concurrent removals of one path
can all report success; a rename succeeds once), so that turn can reach the
model even though its mail has already been confirmed. A genuine prompt consumes the
credit too; a new SessionStart discards an abandoned one. Later consumed pokes
still block. If the credit cannot be stored, startup mail is left unconfirmed
for retry rather than letting the following wake discard it; the mail can be
delivered again after its lease expires. The transient
sidecar holds no message content and changes neither the store schema nor the
relay protocol. Older binaries ignore it. A crash before the next prompt may
leave a sidecar until that session starts again, and a hook killed between
that rename and the removal leaves `<db>.startup-<session hash>.taken-<random>`;
removing either is safe when the session is not running.

Check this sequence against a real Codex host and an isolated local model:

```bash
python scripts/codex-loop-smoke.py --startup-wake-only --pager /path/to/pager --codex /path/to/codex
```

The fixture registers both startup and prompt hooks, queues a wake before the
first model turn, and checks that the model sees the startup mail and replies.
This path was run on Linux with Codex 0.167.0; native macOS/Windows runs remain
pending. The Go credit implementation uses portable filesystem operations.

## Input fields

| Field | Claude Code | Codex | What pager uses it for |
| --- | --- | --- | --- |
| `session_id` | ✅ | ✅ | Session resolution tier 1. Without it the hook does nothing |
| `prompt` | ✅ | ✅ | Deciding a causality reset |
| `cwd` | ✅ | ✅ | Workspace root (the alias isolation axis) |
| `project_dir` | — | ✅ | Workspace root (preferred) |
| `transcript_path` | ✅ | ✅ | Unused (outside phase-one scope) |
| `session_title` | ✅ (once a title is set) | — | Skipping a session title that is already the pager name |

Workspace root resolution order: `project_dir` → `cwd` → `CLAUDE_PROJECT_DIR` →
the process working directory.

## Session title (Claude Code)

On `UserPromptSubmit`, a Claude Code session whose title is not its pager name
gets `hookSpecificOutput.sessionTitle` set to that name, in the same object as
any `additionalContext`:

```json
{
  "hookSpecificOutput": {
    "hookEventName": "UserPromptSubmit",
    "sessionTitle": "nidu"
  }
}
```

Claude Code applies it as a rename, so the title, the transcript's agent name
and the live registry name — the one `ListAgents` and `SendMessage` address —
all become the pager name. Only that event carries it:

- **Not SessionStart.** A SessionStart title moves the title and agent name but
  not the registry name, and by making the title equal it stops every later
  `UserPromptSubmit` from moving the registry name either. A resume restores all
  three from the transcript, so startup is left alone and a new session takes
  the name on its first prompt.
- **`/rename` runs no hook**, so a renamed session is put back on its next
  prompt.
- **Once `session_title` reports the pager name, nothing is emitted** — the
  steady state is as quiet as before.

It is emitted only when `AI_AGENT` starts with `claude-code_` (exported since
Claude Code 2.1.120; the field exists since 2.1.94) **and** the session's stored
tool is `claude`, because `AI_AGENT` is inherited — a Codex started from a
Claude session carries it. Anything else keeps the output it always had.

**Where it does not converge** — if another live Claude session on the same
host already holds the name, Claude Code yields by suffixing it
(`nidu-jaunty-knuth`) and keeps the suffix for the life of that process, while
pager keeps requesting the pager name on each prompt. Resuming the session
after the other one has let the name go puts the pager name back; a fresh
start is a new session with a new pager name. A name Claude Code rewrites when
it normalises a title (a hand-set alias with control characters, or one past
its length limit) never reads back equal either, so it too is requested on
every prompt. The title is never evidence of who owns an address: sends always
resolve through pager.

## What one hook run does

The order is meaningful.

```
1. record session    ← a session nobody has written to still needs an address before an alias can attach
2. issue a name      ← only for sessions without one. Skipped when the workspace is unknown
3. reset causality   ← before delivery. What arrives after the reset causes the new chain
4. query candidates  ← no claim. Outside the transaction
5. select by budget  ← rejects are left untouched (starvation guard)
6. claim the selected ← one BEGIN IMMEDIATE transaction
7. write stdout      ← if a name was just issued, prepend one line saying so. A Claude session title rides in the same object
8. confirm           ← after the write. Dying in between means a retry once the lease expires
9. prune opportunity ← only the one hook that wins the gate
```

**Step 8 coming after step 7 is the crux.** Confirming first would lose the
message outright if the process died before writing.

Step 2 produces a line in step 7 **only when this run is what first gave the
name**. If any later step returns early, that introduction is lost and is not
retried — the name is already stored, so nothing functional breaks, and pager
does not keep extra state just to guarantee exactly-once here. The rules and
reasoning are in [session-binding.md](session-binding.md).

## fail-open

Every path is fail-open — **exit 0, no stdout**.

| Situation | Result |
| --- | --- |
| Malformed JSON / empty input | exit quietly |
| No `session_id` | exit quietly |
| Cannot open the DB / no permission | exit quietly |
| Error during delivery | exit quietly |
| Name issuance failed | ignore and continue (a nameless session still works; the next hook retries) |
| prune failed (including a failed archive write) | ignore and continue, but **it is not retried immediately** — see below |

The cost is that **breakage is invisible from inside the session.** You check
with `pager whoami` (host, session, name), `pager who` (other sessions),
`pager ls` (the queue), and for prune specifically, `pager prune`. A manual
prune bypasses the gate and surfaces errors directly, making it the only window
onto failures the hook swallowed.

### When a failed prune is retried

A failed prune does not update the completion time, but **it has already
recorded its start time before failing.** Winning the gate requires both "the
last completion was 24 hours ago" and "the last start is older than the lease",
so the next run that can take the gate is **the first hook that runs 10 minutes
after that start time.** Every hook in between loses the gate.

Only **automatic prune** is suppressed. The rest of the hook's steps, delivery
included, and a manual `pager prune` are unaffected.

prune deletes only after writing the doomed messages to a file
(`~/.pager/archive.jsonl`), so a failed archive write is a failed prune.
Nothing is deleted and the data stays in the database. Details in
[reference.md](reference.md#retention-and-archive).

One hook run is limited to 5 seconds. Hanging on a locked database while
holding up a session is exactly the situation fail-open exists to prevent.

## Registration

The registration commands are in [../README.md](../README.md#install), and
which events to register is in
[reference.md](reference.md#which-hook-events-to-register).
