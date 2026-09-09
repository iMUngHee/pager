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

Both consequences were bad. A sender could remotely reset a recipient's causal
state, something only the human at the receiving session could do until then;
and with the causal representative gone, the recipient's next reply counted as
hop 0 / `human`, billing against the `maxHumanPerWindow` budget (30) instead of
`maxCausedPerPair` (10). That direction weakens the ping-pong defence.

> This decision used to be the weakest link in the ping-pong defence. The
> assumption written down was "if the host starts injecting synthetic prompts
> when it resumes" — and wake made exactly that real. The sentinel condition
> closes the hole. The remaining line of defence is still the breaker, which
> does nothing but count.

Imitating the sentinel to suppress a reset is possible, but **it works against
the forger.** Suppressed means the reply keeps its causal depth and bills
against the narrower budget — a strengthening, not a weakening, so there is no
reason to abuse it.

## Input fields

| Field | Claude Code | Codex | What pager uses it for |
| --- | --- | --- | --- |
| `session_id` | ✅ | ✅ | Session resolution tier 1. Without it the hook does nothing |
| `prompt` | ✅ | ✅ | Deciding a causality reset |
| `cwd` | ✅ | ✅ | Workspace root (the alias isolation axis) |
| `project_dir` | — | ✅ | Workspace root (preferred) |
| `transcript_path` | ✅ | ✅ | Unused (outside phase-one scope) |

Workspace root resolution order: `project_dir` → `cwd` → `CLAUDE_PROJECT_DIR` →
the process working directory.

## What one hook run does

The order is meaningful.

```
1. record session    ← a session nobody has written to still needs an address before an alias can attach
2. issue a name      ← only for sessions without one. Skipped when the workspace is unknown
3. reset causality   ← before delivery. What arrives after the reset causes the new chain
4. query candidates  ← no claim. Outside the transaction
5. select by budget  ← rejects are left untouched (starvation guard)
6. claim the selected ← one BEGIN IMMEDIATE transaction
7. write stdout      ← if a name was just issued, prepend one line saying so
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
