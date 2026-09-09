// Package wake pokes a recipient session so it reads its mail now rather than
// whenever its user next types.
//
// pager's delivery has never been slow — the write lands immediately. What was
// slow is the recipient looking: hooks fire on human input, so an idle session
// sits on its mail until someone presses enter. wake closes that gap by
// knocking on the host's own message-injection surface, which starts a turn,
// which runs the hook, which delivers. The poke carries no message body; the
// hook remains the only thing that renders and records a delivery.
//
// Every failure here is survivable by construction. The database write already
// happened before wake is called, so a target that cannot be poked simply gets
// its mail the way it always did.
package wake

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/iMUngHee/pager/internal/deliver"
)

// Deadline bounds one whole wake attempt — reaching, connecting, writing.
//
// A wedged surface must not hang the sender: `pager send` and the MCP tool call
// both run this inline. The value matches hookio.Deadline for the same reason
// that one exists.
const Deadline = 5 * time.Second

// retryPause is how long a busy pipe gets before the single retry.
const retryPause = 100 * time.Millisecond

// Outcome is what the caller reports. Callers branch on this and nothing else.
type Outcome string

const (
	// Woken means the kernel accepted the frame. It does NOT mean the peer
	// ran: this protocol has no read step, and the host's delivery receipt
	// goes to a socket pager does not listen on. The recipient's hook is
	// what proves delivery, by recording it.
	Woken Outcome = "woken"

	// Queued means no poke happened and the hook will deliver later. This is
	// the pre-wake behaviour and is never an error.
	Queued Outcome = "queued"

	// Refused means a live peer answered but would not take an authenticated
	// frame. It is reported separately because it is the signal that the
	// host's private contract moved — folded into Queued, nobody would learn
	// that the gate or the key layout changed.
	Refused Outcome = "refused"

	// Disabled means PAGER_WAKE=off. The caller says nothing at all.
	Disabled Outcome = "disabled"
)

// Result carries the outcome plus enough context to diagnose it. Err is for
// humans reading a failure, not for callers to switch on.
type Result struct {
	Outcome Outcome
	Via     string // "uds", "codex-queue", or "" when nothing was tried
	Err     error
}

// waker is one host surface.
//
// reach observes whether this surface can carry a poke to sessionID. It must
// not write anything: it runs against every candidate on every send, including
// the common case where the answer is no.
type waker interface {
	name() string
	reach(sessionID string) bool
	poke(ctx context.Context, sessionID, body string) error
}

// ErrRefused lets an adapter say "a live peer would not take this" rather than
// leaving the caller to infer it from an errno it cannot trust.
//
// The inference is genuinely unavailable: a rejected authentication shows up as
// the server closing the connection, which is the same EPIPE a peer that just
// exited produces. Adapters return this only when they can tell the difference
// from something other than the error value.
var ErrRefused = errors.New("peer refused the poke")

// adapters returns the surfaces to try, in order.
//
// Fresh instances per call: an adapter may cache what it resolved between
// reach and poke, and that cache belongs to one Wake, not to the process.
//
// It is a variable so tests can substitute stubs. Substituting is mandatory in
// any test that calls Wake, because the real adapters read the surrounding
// host.
var adapters = func() []waker { return []waker{newClaudeWaker(), newCodexWaker()} }

// Enabled reports whether wake may run at all.
//
// Default on: with no surface registered for a target the cost is one directory
// read. Off is for tests that must not touch the surrounding host, and for
// anyone who wants the old behaviour back without rebuilding.
func Enabled() bool { return os.Getenv("PAGER_WAKE") != "off" }

// Wake tries to poke sessionID and never fails the send.
//
// Order is observation, not configuration: each adapter is asked whether it can
// reach this session. Nothing consults the tool recorded for the session — the
// call site does not even have it, since ResolveTarget returns only an alias and
// a session id.
func Wake(ctx context.Context, sessionID, body string) Result {
	if !Enabled() {
		return Result{Outcome: Disabled}
	}
	if sessionID == "" {
		return Result{Outcome: Queued}
	}

	ctx, cancel := context.WithTimeout(ctx, Deadline)
	defer cancel()

	for _, w := range adapters() {
		if !w.reach(sessionID) {
			continue
		}
		err := pokeWithRetry(ctx, w, sessionID, body)
		if err == nil {
			return Result{Outcome: Woken, Via: w.name()}
		}
		return Result{Outcome: classify(err), Via: w.name(), Err: err}
	}
	return Result{Outcome: Queued}
}

// pokeWithRetry gives a momentarily busy pipe one more chance.
//
// Only busy is retried. A peer that is gone will still be gone, and a refusal
// will still refuse; retrying either would just spend the caller's deadline.
func pokeWithRetry(ctx context.Context, w waker, sessionID, body string) error {
	err := w.poke(ctx, sessionID, body)
	if err == nil || !isBusy(err) {
		return err
	}
	select {
	case <-ctx.Done():
		return err
	case <-time.After(retryPause):
	}
	return w.poke(ctx, sessionID, body)
}

func isBusy(err error) bool {
	return errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EAGAIN)
}

// classify maps a poke failure onto what the sender should be told.
//
// Almost everything is Queued, and that is the point: the mail is already
// stored, so a failed poke costs latency rather than delivery. Only two things
// are worth separating — an explicit refusal, and the permission boundary,
// because both mean retrying will not help and something outside pager changed.
func classify(err error) Outcome {
	switch {
	case errors.Is(err, ErrRefused), errors.Is(err, syscall.EACCES):
		return Refused
	default:
		return Queued
	}
}

// PokeBody is the one line the recipient sees before its hook renders the mail.
//
// It deliberately carries no message text. The hook renders the batch on the
// same turn, with the budget and truncation rules that live in one place, and a
// second copy here would be a second delivery path.
//
// The sentinel keeps this from resetting the recipient's causal chain; see
// deliver.PokeSentinel. The recovery hint names msg_list rather than `pager ls`
// because only msg_list records that the mail was read — following a hint to
// `pager ls` would show the message and then have the next hook inject it
// again.
//
// tool is the sender's host ("claude" or "codex") and may be "". It is named
// because the receiving host wraps the poke in its own framing — Claude Code
// says "another Claude session sent a message" whatever the sender runs under —
// so without it a Codex sender reads as Claude.
func PokeBody(alias, label, tool string) string {
	from := label
	if from == "" {
		from = "another session"
	}
	if tool != "" {
		from += " (" + tool + " session)"
	}
	return "pager: new mail for " + alias + " from " + from + ". " + deliver.PokeSentinel +
		" If it is not shown with this turn, run msg_list (or `pager ls`, which shows it without marking it read)."
}

// Describe renders the outcome for a human, or "" when there is nothing to say.
//
// Every line here says what this process observed and stops there. Writing the
// frame is the whole of that observation — there is no read step and the
// host's receipt goes elsewhere — so no line may say what the peer then did.
// TestDescribeDoesNotClaimThePeerActed pins the Woken line for that reason.
func Describe(r Result, alias string) string {
	switch r.Outcome {
	case Woken:
		return "poked " + alias + " (" + r.Via + ") — written to its inbox; its own hook is what delivers"
	case Refused:
		return "note: " + alias + " has a live inbox but refused the poke (" + r.Via +
			") — it will be delivered on its next activity. This usually means the host's messaging contract changed."
	case Queued:
		return "note: " + alias + " was not poked — it will be delivered on its next activity"
	default: // Disabled
		return ""
	}
}
