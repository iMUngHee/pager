// Package hookio adapts pager to the hook contract Claude Code and Codex
// share: a JSON payload on stdin, and an optional
// hookSpecificOutput.additionalContext object on stdout that the host folds
// into the agent's next turn. For Claude Code the same object can also carry
// sessionTitle, which keeps the session named after its pager name.
//
// Errors here fail open. A hook that exits non-zero or prints a
// diagnostic can stall or pollute the session it was meant to serve, and a
// paging system that breaks the sessions it pages is worse than one that
// silently delivers nothing. The cost is that a genuinely broken pager is
// invisible from inside the session — `pager whoami` and `pager ls` are how it
// becomes visible.
// A successfully checked, already-consumed wake prompt is intentionally
// blocked so the host does not spend a model turn on an empty notification.
package hookio

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/iMUngHee/pager/internal/clock"
	"github.com/iMUngHee/pager/internal/deliver"
	"github.com/iMUngHee/pager/internal/sessionref"
	"github.com/iMUngHee/pager/internal/store"
	"github.com/iMUngHee/pager/internal/wake"
)

// Hook events pager reacts to.
const (
	EventUserPromptSubmit = "UserPromptSubmit"
	EventSessionStart     = "SessionStart"
	EventStop             = "Stop"
	EventSubagentStop     = "SubagentStop"
)

// Input is the subset of the payload pager reads. Both tools send session_id;
// Codex sends project_dir or cwd, while Claude Code also exports
// CLAUDE_PROJECT_DIR.
type Input struct {
	SessionID      string `json:"session_id"`
	HookEventName  string `json:"hook_event_name"`
	ProjectDir     string `json:"project_dir"`
	CWD            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
	// SessionTitle is the title Claude Code currently shows. It is absent
	// until one is set, and absent altogether on versions that predate it.
	SessionTitle string `json:"session_title"`
}

// output is the object both hosts accept. sessionTitle is Claude Code's alone
// and is only ever set where sessionTitleFor allows it.
type output struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext,omitempty"`
		SessionTitle      string `json:"sessionTitle,omitempty"`
	} `json:"hookSpecificOutput"`
}

// Run performs one hook invocation and reports nothing to its caller: the
// process should exit 0 whatever happened.
//
// The order is deliberate. Recording the session comes first so that a session
// which has never been paged still becomes addressable. Writing to stdout comes
// before confirming, so a crash in between leaves the messages claimed but
// unconfirmed and they are retried once the lease lapses — the alternative,
// confirming first, would lose them outright.
func Run(ctx context.Context, event string, stdin io.Reader, stdout io.Writer) {
	in, ok := parse(stdin)
	if !ok {
		return
	}
	if event == "" {
		event = in.HookEventName
	}
	event = canonicalEvent(event)
	if in.SessionID == "" {
		return // nothing to attribute this run to
	}

	path, err := store.DefaultPath()
	if err != nil {
		return
	}
	st, err := store.Open(ctx, path, clock.System{})
	if err != nil {
		return
	}
	defer st.Close()

	client, host, _ := sessionref.Detect()
	rec := store.SessionRecord{
		ID: in.SessionID, Tool: client, Root: workspaceRoot(in),
		HostClient: client, HostPid: host.Pid, HostStart: host.Start,
	}
	if rec.Tool == sessionref.Unknown {
		rec.Tool = ""
	}
	// The roster's PURPOSE column, written from the prompt this hook already
	// carries. startsNewTurn is reused rather than restated: it is exactly the
	// question being asked — is this a prompt of the user's own — and a second
	// spelling of "not a poke" is how the two would drift apart. It now has two
	// consumers, the causal epoch and this; narrowing it narrows both.
	//
	// An empty purpose leaves the stored value alone, which is what carries it
	// across the SessionStart and Stop events that have no prompt at all.
	if startsNewTurn(event, in) {
		rec.Purpose = deliver.Purpose(in.Prompt)
	}
	if err := st.RecordSession(ctx, rec); err != nil {
		return
	}

	// Give the session a name if it does not have one. Until it does, anything
	// it sends is attributed to a session id, which is what a recipient saw
	// before this existed: a UUID with no way to tell who sent it.
	//
	// The error is dropped like every other one here. An unnamed session still
	// works — it is only harder to talk about — and a hook that fails loudly
	// costs the session it was meant to serve.
	name, named, _ := deliver.EnsureAutoAlias(ctx, st, in.SessionID)

	// Codex runs SessionStart before UserPromptSubmit in the same turn. Mail
	// delivered by startup has not reached the model yet. Permit the following
	// prompt once, and discard abandoned credit at the next startup.
	var startupMail, codexStartup bool
	var creditErr error
	if event == EventSessionStart || event == EventUserPromptSubmit {
		if _, tool, err := deliver.SessionWorkspace(ctx, st, in.SessionID); err == nil && tool == sessionref.Codex {
			codexStartup = event == EventSessionStart
			startupMail, creditErr = startupDeliveryCredit(path, in.SessionID, false)
		}
	}

	// A user prompt ends whatever exchange preceded it, which is what keeps
	// ordinary work from accumulating causal depth. See startsNewTurn for what
	// "a user prompt" is allowed to mean.
	if startsNewTurn(event, in) {
		if err := deliver.ResetCausal(ctx, st, in.SessionID); err != nil {
			return
		}
	}

	// Queue entries can outlive the mail: a poll or an earlier hook may have
	// consumed it while this poke waited behind a running turn. Check before
	// collect, which intentionally re-injects polled messages on normal turns.
	// Only a standalone wake prompt may be dropped, never quoted user text.
	if event == EventUserPromptSubmit && deliver.IsPokePrompt(in.Prompt) {
		pending, err := deliver.List(ctx, st, in.SessionID, deliver.FilterWaiting, deliver.LimitsFromEnv())
		if err == nil && len(pending) == 0 && !startupMail && creditErr == nil {
			_ = json.NewEncoder(stdout).Encode(struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
			}{"block", "pager: this wake notification has already been handled; no unread mail remains."})
			return
		}
	}

	text := collect(ctx, st, in.SessionID, event)
	// The introduction goes first: it explains the name that the messages
	// below it are addressed to.
	if named {
		text.body = deliver.RenderIntroduction(name) + text.body
	}
	title := sessionTitleFor(ctx, st, event, in, name)
	if text.body != "" || title != "" {
		if err := write(stdout, event, text.body, title); err != nil {
			return
		}
	}
	if text.token != "" {
		if codexStartup {
			if _, err := startupDeliveryCredit(path, in.SessionID, true); err != nil {
				// Without the credit, confirming would let the next prompt block
				// this delivery. Keep it unread; its lease can be retried instead.
				return
			}
		}
		// Best-effort: the messages have already been written out, and an
		// unconfirmed claim is retried rather than lost.
		if _, err := deliver.ConfirmDelivery(ctx, st, in.SessionID, text.token); err == nil {
			_ = wake.ClearCodexQueue(ctx, st, in.SessionID)
		}
	}

	// Housekeeping rides along on whichever hook wins the gate.
	_, _ = st.PruneOpportunistic(ctx, store.DefaultRetention)
}

// startupDeliveryCredit records or atomically consumes one startup delivery.
// The empty sidecar is isolated by database and exact session, so this
// transient handshake needs no store migration and holds no message content.
//
// Consuming renames the sidecar to a name only this call knows, then removes
// that. os.Remove alone is not an atomic take: on macOS (APFS) concurrent
// unlinks of one path can each report success. The removal that follows the
// rename races nobody, and only the call whose name still holds the file can
// remove it, so at most one call takes the credit. What cannot be removed is
// put back, so it stays in the way of the next startup's credit as before.
func startupDeliveryCredit(path, session string, remember bool) (bool, error) {
	marker := fmt.Sprintf("%s.startup-%x", path, sha256.Sum256([]byte(session)))
	if !remember {
		claim := marker + ".taken-" + rand.Text()
		err := os.Rename(marker, claim)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err := os.Remove(claim); err != nil {
			_ = os.Rename(claim, marker)
			return false, err
		}
		return true, nil
	}
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	return true, f.Close()
}

type injection struct {
	body  string
	token string
}

// collect assembles what this event is allowed to say.
//
// The orphan hint is confined to UserPromptSubmit and SessionStart. Output from
// a Stop hook continues the conversation, so emitting a hint there would spend
// an agent turn to deliver nothing — the session would wake up, read a
// suggestion about an inbox, and have no message to act on.
//
// The workspace pair is read here rather than passed in, and read after the
// event gate rather than before it. Reading it at all is what makes the hint
// survive a failed detection — see deliver.SessionWorkspace. Reading it after
// the gate is what keeps the two events that can never show a hint from paying
// for the lookup.
func collect(ctx context.Context, st *store.Store, session, event string) injection {
	var out injection

	batch, err := deliver.CollectBatch(ctx, st, session, deliver.LimitsFromEnv())
	if err == nil && !batch.Empty() {
		out.body = deliver.Render(batch)
		out.token = batch.Token
	}

	if event != EventUserPromptSubmit && event != EventSessionStart {
		return out
	}
	root, tool, err := deliver.SessionWorkspace(ctx, st, session)
	if err != nil || root == "" || tool == "" {
		return out
	}
	orphans, err := deliver.OrphanAliases(ctx, st, root, tool, store.DefaultStale)
	if err != nil || len(orphans) == 0 {
		return out
	}
	// One suggestion per session: reserved rather than merely checked, so
	// concurrent hooks cannot each decide they are the first.
	shown, err := deliver.TakeClaimHint(ctx, st, session)
	if err != nil || !shown {
		return out
	}
	if out.body != "" {
		out.body += "\n"
	}
	out.body += deliver.RenderOrphanHint(orphans)
	return out
}

// startsNewTurn reports whether this event represents a person taking the
// conversation somewhere new.
//
// The test is structural: the event is UserPromptSubmit, the payload carries a
// non-empty prompt, and that prompt is not one of pager's own wake pokes. An
// automatic continuation — a host resuming itself after a Stop hook wrote
// context — carries no prompt text of its own, so it does not reset the chain
// and a reply it produces still counts as caused.
//
// The poke exclusion closes what used to be the weakest link here. The comment
// this replaces warned that a host synthesising a non-empty prompt would reset
// chains one step early; wake made that hypothetical real, because a poke is
// delivered through the host's injection path and reaches the hook looking
// exactly like typed input. Measured before the exclusion existed: one poke
// moved the recipient's causal epoch and cleared its inbound pointer, so the
// reply it prompted would have lost its causal attribution.
// Mail arriving is not a person taking the conversation somewhere new.
//
// Only a complete generated notification gets this treatment. Quoted markers
// and notifications with appended user instructions retain normal user intent.
func startsNewTurn(event string, in Input) bool {
	if event != EventUserPromptSubmit {
		return false
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return false
	}
	return !strings.Contains(prompt, deliver.PokeSentinel)
}

// canonicalEvent maps an event name onto its canonical spelling, ignoring case.
//
// Registrations are written by hand, next to other tools' hooks that spell the
// same events in lowercase, so the wrong case is a matter of when rather than
// if. Matching exactly would fail in the worst possible way: delivery keeps
// working, so the registration looks correct, while the causal reset silently
// stops happening and depth accumulates until sends start being refused days
// later. Normalising here also means the emitted hookEventName carries the
// spelling the host expects rather than whatever was typed.
func canonicalEvent(event string) string {
	for _, known := range []string{EventUserPromptSubmit, EventSessionStart, EventStop, EventSubagentStop} {
		if strings.EqualFold(event, known) {
			return known
		}
	}
	return event
}

// workspaceRoot resolves the workspace this session belongs to. Codex puts it
// in the payload; Claude Code exports it.
func workspaceRoot(in Input) string {
	for _, candidate := range []string{in.ProjectDir, in.CWD, os.Getenv("CLAUDE_PROJECT_DIR")} {
		if candidate != "" {
			return candidate
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

// parse reads the payload, tolerating anything that is not the shape expected.
func parse(r io.Reader) (Input, bool) {
	raw, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return Input{}, false
	}
	var in Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return Input{}, false
	}
	return in, true
}

// sessionTitleFor returns the title to hand Claude Code, or "" to leave it.
//
// Setting it from UserPromptSubmit makes Claude Code apply it as a rename,
// which moves the title, the transcript's agent name and the live registry
// name — the one ListAgents and SendMessage address — onto the pager name.
// SessionStart would only move the first two, and by making the title equal it
// would stop every later UserPromptSubmit from moving the third, so startup is
// deliberately left alone; a resume restores all three from the transcript.
// /rename runs no hook, so a renamed session is put back on its next prompt.
//
// AI_AGENT is how the host is recognised as Claude Code: it has been exported
// since 2.1.120, and the field since 2.1.94, so a host that sets it can take
// the title and one that does not keeps the output it always had. The stored
// tool is checked as well because AI_AGENT is inherited — a Codex started from
// a Claude session carries it, and Codex has no such field. A Claude Code
// older than 2.1.120 started inside a newer one would inherit it too; that
// version still knows the field (2.1.94+) unless it is older than that, a
// nesting this does not guard against.
//
// A title the host reports back as current needs nothing. One that stays
// different — Claude Code yields a name another live session on the host
// holds by suffixing it, and keeps the suffix for the life of the process — is
// requested again on each prompt until a resume after the holder has let the
// name go.
func sessionTitleFor(ctx context.Context, st *store.Store, event string, in Input, name string) string {
	if event != EventUserPromptSubmit || name == "" || in.SessionTitle == name {
		return ""
	}
	if !strings.HasPrefix(os.Getenv("AI_AGENT"), "claude-code_") {
		return ""
	}
	if _, tool, err := deliver.SessionWorkspace(ctx, st, in.SessionID); err != nil || tool != sessionref.Claude {
		return ""
	}
	return name
}

func write(w io.Writer, event, context, title string) error {
	var out output
	out.HookSpecificOutput.HookEventName = event
	out.HookSpecificOutput.AdditionalContext = context
	out.HookSpecificOutput.SessionTitle = title
	enc := json.NewEncoder(w)
	return enc.Encode(out)
}

// Deadline bounds one hook invocation. A hook blocking on a wedged database
// would stall the session, which fail-open exists to prevent.
const Deadline = 5 * time.Second
