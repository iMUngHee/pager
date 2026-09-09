# pager reference

[English](reference.md) · [한국어](reference.ko.md)

> Installing and sending your first message are in [../README.md](../README.md).
> This document covers what you need after that: the conditions delivery
> depends on, configuration, what each column means, and why each choice was
> made.
>
> See also: [hooks.md](hooks.md), [session-binding.md](session-binding.md)

## When it arrives — two paths

Sending is always immediate. What varies is when the other side looks, and
there are two ways that happens.

```
if the recipient is alive and its host exposes a way in
    send knocks on the spot → it takes a turn shortly, and that turn's hook delivers
    measured: claude UDS 0.8s, codex queue 1-8s

otherwise (nobody there, the gate is off, or the knock failed)
    the original behaviour → its next activity runs a hook, and that hook delivers
```

**The knock carries no content.** A poke is one line saying "you have mail";
delivery is always the hook's job. Rendering, budgeting and read-tracking
therefore live in exactly one place, and a failed knock costs latency and
nothing else.

`pager send` tells you which path it took.

```
poked <name> (uds) — written to its inbox; its own hook is what delivers
note: <name> was not poked — it will be delivered on its next activity
```

`woken` means **the kernel accepted the frame**, not that the recipient ran.
That path has no read step. Whether it actually landed is answered by
`delivered_at` on the receiving side.

### When nobody is behind the name

Before reporting the knock result, pager checks something else first: is the
process of the session holding that name still running?

```
note: <name>'s session is gone — the message waits, but nothing will read it
      until a session in that workspace claims the name; pager who lists who can answer now
```

An alias outlives its session — that is the property that makes handover
possible. So a dead inbox still resolves cleanly by name, and the sender
believes somebody is there. Reporting only the knock result would print `was
not poked — it will be delivered on its next activity`, and **there is no next
activity.** That sentence is true about the queue and false about the reader,
so it reassures at exactly the moment nobody is home.

The verdict uses **pid plus a process start token** (`sessionref.Alive`), not a
heartbeat — the same reason the `HOST` column in `pager inbox` does. And
**pager does not claim absence when it cannot check the host.** A session whose
host detection failed at attach time is unknown, not dead, and asserting
absence without grounds would hide a live notification. The line appears only
when pager is sure.

The CLI and MCP `msg_send` print the **same sentence**, from one place
(`deliver.Presence.Note`).

## The guarantee

There is a reason this is not called "at-least-once". If the premises below
break, **the number of output attempts can be zero.**

```
guaranteed:  if the recipient's hook runs within PAGER_INJECT_TTL and the stdout
             write succeeds, then even if the process dies before confirming,
             the message is retried once the lease expires.

premises:    the recipient's hook runs at least once within the TTL  ← a landed knock
                                                                      satisfies this at once;
                                                                      without one, an idle
                                                                      session breaks it
             the hook can reach the database                         ← fail-open, so it
                                                                      fails silently
             the host adopts additionalContext                        ← the hook contract has
                                                                      no ack, so this is
                                                                      unverifiable

not guaranteed: if the host discards the output after confirmation, there is no redelivery.
stops:          automatic retry stops once the TTL has passed.
```

**The knock does not change this guarantee.** It only makes the first premise
hold far more often.

**Seeing a message twice is the price of retry.** The message id in the
injected text is an observability aid, not durable dedup.

## Which hook events to register

The registration snippets are in [../README.md](../README.md#install). What
follows is why they look the way they do.

**Prefer the form with no event name argument (`pager hook`).** It reads
`hook_event_name` from the payload, so it cannot be misspelled. If you want it
explicit, `pager hook UserPromptSubmit` works, and **case does not matter** —
these entries commonly sit next to other tools' config that spells the same
events in lowercase.

The inner timeout is fixed at 5 seconds. Give the outer timeout more room than
that (Claude uses milliseconds, Codex uses seconds).

| Event | Grade | If you leave it out |
| --- | --- | --- |
| `UserPromptSubmit` | **required** | Causality never resets, so hop depth accumulates forever. Session recording and delivery also depend on it |
| `SessionStart` | recommended | A new session does not see waiting mail or the orphan-inbox hint until its first prompt |
| `Stop` | retired | Its only reason to exist was "deliver right after a turn ends", and the knock does that instead. It still works if registered, but there is no reason to add it now |

> **Do not register Stop on Codex.** Whether the Codex Stop hook adopts
> `additionalContext` is unverified. The path verified on both tools is
> `UserPromptSubmit`.

## `PURPOSE` in the MCP roster

Registration commands are in
[../README.md](../README.md#using-it-from-inside-an-agent-optional). Of the
three tools, `msg_roster` is the only one that shows something the CLI does
not: the same columns as `pager who` plus **one more, `PURPOSE`**.

```
NAME  TOOL    ROOT       HOST  LAST      PURPOSE
gupa  claude  ~/.config  live  just now  rebuild the tmux badge
suzu  claude  ~/.config  live  4m ago    add zsh completions
```

- **`PURPOSE` is the last human instruction that session received.** The
  `UserPromptSubmit` hook folds the prompt to one line, stores up to 80
  characters, and refreshes it on every prompt. If `ROOT` is "where it is",
  this is "what it is doing" — and **when two sessions share a repository,
  there is nothing else to tell them apart.**
- **Messages pager sends to wake a session do not become `PURPOSE`.**
  Otherwise the busiest inboxes would all read "check the pager message".
- A session where nobody has typed a prompt yet shows blank.
- **Every agent session on this machine can read this value.** If you call
  `msg_roster`, you will see instructions typed in other projects too. And
  since no code deletes `sessions` rows (`prune` only touches messages), values
  from dead sessions stay in the file. This is a deliberate choice, resting on
  the premise that a local database's trust model is the same one
  `pager ls --session` already assumes.
- It does not appear in `pager who`. That table already wraps at 80 columns,
  and one more column costs the human reader more than it gives.

## Every command

The README table covers the common ones. This is all of them.

```bash
pager who                          # sessions you can call + whether each host is alive
pager send hica "parser is yours"  # send
pager ls                           # what came to me (including already-read)
pager ls --waiting                 # only what nobody has picked up — for mid-task checks
pager ls --expired                 # past the automatic delivery window and still untouched
pager inbox                        # every inbox with mail waiting (session-independent)
pager whoami                       # my session and my name
pager alias review-box             # choose a name (in place of the automatic one)
pager claim review-box             # take over an offline inbox
pager prune --dry-run              # what is past retention
pager export > backup.jsonl        # the whole store as JSONL
```

**"Picked up" means two things at once.** Either a hook injected the message
into a session's context (STATE `delivered`), or an agent looked at it directly
through MCP `msg_list` (STATE `seen`). `--waiting` and `--expired` list only
what has neither. So `--waiting` is not "what **I** haven't seen" but "what
**nobody** has touched yet". `pager ls` records nothing — reading someone
else's inbox with `ls --session` does not consume their mail — so the list a
human reads is unaffected by this distinction.

A message read by polling is **injected again by the hook on the next turn.**
That is intent, not a bug: context may have been compacted in between, so "who
saw it" and "it is in context now" are not the same statement.

### `pager who` — who you can send to

```
NAME  TOOL    ROOT                      HOST     LAST
gupa  claude  ~/Projects/portal/main    live     just now
suzu  codex   ~/.config                 gone     8m ago
hica  claude  ~/Projects/pager          unknown  21m ago
```

This is the **same table** MCP `msg_roster` produces. Rendering comes from one
place, so the two surfaces cannot answer the same question differently.

- **Being listed and being alive are different things.** Listing is based on
  `heartbeat_at` with a 12-hour default threshold, so a session that died
  minutes ago stays listed for half a day (measured: 4 of 8 rows). That is why
  `HOST` exists separately — the verdict and the meaning of its three words are
  described under `pager inbox` below.
- `LAST` is only when the last hook ran. A session in a long turn looks stale,
  and a session that just died looks recent. **Decide whether to send from
  `HOST`.**
- Dead sessions are not hidden. An inbox with nobody behind the name is what
  `pager claim` is for, and hiding it would hide the candidate.

### `pager inbox` — what is waiting, everywhere

If `ls` answers "what came to **me**", `inbox` answers "who is waiting on mail
**right now**". It resolves no session and looks at the whole store. Inboxes
with zero waiting are omitted; if there are none, it prints `nothing waiting`.

```
INBOX  WAITING  HOST
gupa   3        live
suzu   1        gone
hica   2        unknown
```

**These three columns are the contract external consumers may parse.** They
exist so that something needing a global view — a tmux status badge, say —
does not have to read the SQLite schema directly. Keeping the column count low
is how that contract stays keepable.

- `WAITING` is **by definition** the number `ls --waiting` counts for that
  inbox. They share the same predicate. Something past the automatic delivery
  window is still waiting (`expired ⊂ waiting`).
- `HOST` is decided by **pid and process start token**. `live` means the
  recorded process is still running, `gone` means it is definitely not, and
  `unknown` means there is no pid to ask about (host detection failed at attach
  time, or the alias outlived its session). **Do not treat `unknown` as
  `gone`** — that hides mail nobody has abandoned.
- `HOST` is **not a heartbeat.** `heartbeat_at` only advances when a hook runs,
  and hooks run at turn boundaries, so a session in a long turn looks stale
  while working perfectly (measured: 7 minutes). "Have we heard from this
  session recently" is a different question, and `who`'s `LAST` column answers
  it.
- **Whether to hide dead inboxes is the consumer's call.** pager only reports:
  a notifier wants to drop them, but an audit wants exactly those.

`pager attach` is handled by the hook — you need it only when writing by hand
without registering hooks.

**Flags may come before or after.** `pager send hica "body" --human` and
`pager send --human hica "body"` are the same. To put a word identical to a
flag name in the body, cut with `--`:
`pager send hica -- "--human is what I want to write"`. Everything after `--`
is body. An undeclared word starting with `-` is body already, so it needs no
cut.

A session **is addressed by alias.** No alias, no inbox.

### Names are assigned automatically

After one hook run, a session gets a **four-letter name** like `hica`. It
alternates consonants and vowels so it is pronounceable, and it **means
nothing** — there is no reason for it to mean anything, and giving it meaning
would require choosing what to derive it from, a choice that silently points at
the wrong session when wrong.

Without a name, the sender label becomes a session UUID and the recipient has no
way to tell who sent it. So names are assigned without being asked for. Use
`pager who` for other names and `pager whoami` for your own.

Two syllables give 8,100 names, which is plenty for the handful of sessions one
person has open. But names are never reclaimed, so the space fills up over
years — after eight consecutive collisions the generator moves to three
syllables (six letters, 729,000 names) rather than quietly giving up and
leaving a session showing a UUID.

- **A chosen name wins.** After `pager alias review-box`, that session is
  `review-box`. The automatic name still works as an address — the session
  simply has more than one.
- **No name is issued when the host cannot be detected.** An alias copies and
  pins the workspace it was created in, so attaching one while unknown binds
  that name to the wrong workspace forever. Staying nameless and waiting for
  the next chance is better.
- **Names are never reclaimed automatically.** A name given is that session's,
  and the system will not take it away for someone else. A human taking it with
  `pager claim` works as always.

A target resolves in this order:
`exact alias → session id → pm_ref suffix → unique substring among live → unique substring overall`

**Calling by session id or pm_ref is unambiguous** even when the session has
several names — there is no wrong session to reach, so they fold to a
representative name. It does fail if different sessions match (two sessions
holding the same pm_ref). **If several substrings match**, there is no way to
know which name you meant, so it shows the candidates and fails.

**Abbreviations look at live names first.** Since names are never reclaimed,
finished sessions keep piling names up, and taking them all on at once would
make it impossible to abbreviate a name you just read from `pager who`. If it
is unique among live names, that settles it; only when none match does the
search widen to everything — which is why **you can still call a departed
session's name.** Mail you leave for a handover does not lose its destination.

### Aliases do not transfer automatically

If today's session inherited yesterday's alias in the same repository, that is
a misdelivery. Handover is **explicit**, via `pager claim`. When there is an
inbox available to take over, the hook mentions it once per session.

## Environment variables

| Variable | Default | Meaning |
| --- | --- | --- |
| `PAGER_DB` | `~/.pager/msg.db` | Store location. For isolated experiments |
| `PAGER_SESSION` | — | Session resolution tier 2. Planted by the hook, inherited by child processes |
| `PAGER_CLIENT` | auto-detected | Pin the host tool (`claude`/`codex`). **An unrecognized value means "no host"** |
| `PAGER_MAX_INJECT_BYTES` | `2000` | UTF-8 bytes for the whole injected text |
| `PAGER_MAX_BATCH` | `5` | Most messages injected at once |
| `PAGER_MAX_BODY_RUNES` | `280` | Body truncation point (in characters) |
| `PAGER_INJECT_TTL` | `72h` | Automatic delivery window. Past it, retry stops (nothing is deleted) |
| `PAGER_LEASE` | `2m` | How long a claim is held |
| `PAGER_ARCHIVE` | on | `off` deletes without archiving. **Any value other than `off` is on** |
| `PAGER_WAKE` | on | `off` does not knock — delivery depends on hooks alone, the older behaviour. **Any value other than `off` is on** |

Leaving `PAGER_WAKE` on costs one directory read when no surface is available.

**Knocking claude requires the receiving session to be running with
`CLAUDE_CODE_HARBOR_KITE=1`.** pager neither sets nor requires this value; it
only observes whether the socket is registered. Without it, delivery falls back
to the hook path. Putting it in the `env` block of `~/.claude/settings.json`
applies it to every new session. It is an unannounced flag, so it may disappear
in a release — and if it does, only knocking stops, silently.

**codex needs no extra setting.** `codex queue` is a public CLI. But a thread
must have a rollout, so **a session that has never run a turn cannot be
knocked.**

Every default is an **estimate**, chosen without production metrics.

## Retention and archive

Retention is 30 days, after which messages are deleted. One hook run performs
this once every 24 hours; `pager prune` runs it directly.

**Nothing is deleted before it is on disk.** prune appends each row it is about
to delete to `~/.pager/archive.jsonl` and `fsync`s **before** deleting. If the
file cannot be written, nothing is deleted — a full disk turning into permanent
data loss is precisely what this is guarding against. The cost is that
**prune stops making progress while the write keeps failing, so the database
grows**, and that was judged better than silent loss. The symptom is
`pager prune` exiting with an error; the escape hatch is `PAGER_ARCHIVE=off`.

The archive file and `pager export` use the **same record format**, so
filtering is `jq`'s job and saving is the shell's — `pager export` has no
flags.

```bash
pager export | jq 'select(.alias == "hica")'
cat ~/.pager/archive.jsonl <(pager export) | awk '!seen[$0]++'   # full history, deduped
```

**Duplication is the contract — at-least-once.** Two paths can put the same
message in more than once: the process dying between fsync and delete, and a
row skipped by that delete because a reply referenced it being written again on
the next pass.

The `awk` above folds **only byte-identical lines**. A duplicate from retry is
the same row re-encoded, so its bytes match, while two different messages differ
in time and body and both survive. This is **a convenience, not a guarantee** —
delete and recreate the database and ids restart from 1, so two records
identical in every field could in principle fold into one. The archive file
itself always preserves every line.

**Two prunes serialize against each other.** The prune gate holds two things:
the once-per-24-hours interval, and the exclusion that stops two from running
at once. A manual `pager prune` bypasses only the interval and keeps the
exclusion. If a prune is already running, it does nothing and says so.

When the interval was bypassed too, two prunes **computed the same offset in
the archive file and overwrote each other.** The archive append is a file write
outside the transaction, and it writes at an absolute offset rather than with
`O_APPEND`. The result then was not a corrupt line but **silent loss** — the
file parses perfectly, records are simply gone, and those rows are deleted from
the database. A failed manual prune releases the lease immediately, so you can
fix the cause and run it again right away.

**The format is a contract.** Each line carries `"v":1`. Adding fields leaves
it alone; removing, renaming or changing the meaning of a field bumps `v`.
Nullable fields are written as `null` rather than omitted, and times are UTC
RFC3339 with milliseconds fixed at three digits. Delivery-lease bookkeeping
(`claim_*`) is not included.

One limitation: bodies are text other agents wrote, and SQLite does not
validate UTF-8. Invalid byte sequences are replaced with U+FFFD during
encoding, so **losslessness holds only for valid UTF-8 bodies.**

## `--human` is a declaration, not a check

`pager send --human` is **the caller's claim** that a human is sending this,
and pager has no way to confirm it. An automated agent can attach it too, and
doing so breaks the hop chain.

So **the last line of defence is the breaker**, which does not read claims. It
only counts.

| Limit | Default |
| --- | --- |
| automated sends / session / hour | 20 |
| automated sends / inbox pair / hour | 10 |
| uncaused sends (including `--human`) / hour | 30 |

Hop depth guards ping-pong as well. A send caused by a received message is one
deeper, and past 3 it is refused. **A user prompt breaks the chain** — which is
why ordinary back-and-forth work never accumulates depth.

When refused, the thing to do is **raise it to the user**. Retrying gives the
same answer, and `--human` is a human's declaration, not a bypass. Being
blocked is itself information: it means agents went four steps without a human,
and a human knowing that is the point of the limit. The error text says all
three of these.

There are two ways back to a shallow chain: a user prompt, and **receiving a
shallower message.** The causal representative is overwritten by the batch's
maximum depth rather than taking the max with the existing value, so a hop-0
message arriving after a deep chain ends returns the next send to hop 1.

## Trust boundary

A message body is **text another agent wrote entering your context** — a
prompt-injection path. It is fundamentally data crossing a trust boundary,
which cannot be removed, only reduced:

- the sender is always shown
- the body is wrapped as a quote block and marked data, not instructions
- length is capped

The store is local to one user account with no network exposure.

## When it breaks

**Every hook path is fail-open** — exit 0, no stdout. A hook failure blocking a
session is worse than a delivery failure. The cost is that **breakage is
invisible from inside the session.**

```bash
pager whoami     # is the host detected, is a session bound, what is my name
pager who        # are other sessions visible
pager ls         # what is in the queue
pager inbox      # which inboxes have mail piling up, and is a process alive behind them
pager prune      # is the archive write failing (it fails silently in hooks)
```

`session: unattributed` means the hook has not recorded this session yet, or
host detection failed. `name: none` means the session is recorded but got no
name, usually for the same reason — failed host detection. Name issuance is
fail-open too, so it says nothing when it fails, and **these two commands are
the only window onto that.** Details in
[session-binding.md](session-binding.md).

## Out of scope

**pager does not build interrupts.** What it automates is "a human copying text
across", not "a human noticing". FIFO blocking, tmux send-keys and transcript
editing were all considered and dropped: all three have to touch the recipient
session's in-progress turn.

A tmux status badge and an overlay TUI are later steps.

## Other documents

- [../README.md](../README.md) — install and first use
- [session-binding.md](session-binding.md) — the four resolution tiers, host
  detection, isolation rules
- [hooks.md](hooks.md) — event / input / output matrix, the fail-open contract
