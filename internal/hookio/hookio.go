// Package hookio adapts pager to the hook contract Claude Code and Codex
// share: a JSON payload on stdin, and an optional
// hookSpecificOutput.additionalContext object on stdout that the host folds
// into the agent's next turn.
//
// Every path here fails open. A hook that exits non-zero or prints a
// diagnostic can stall or pollute the session it was meant to serve, and a
// paging system that breaks the sessions it pages is worse than one that
// silently delivers nothing. The cost is that a genuinely broken pager is
// invisible from inside the session — `pager whoami` and `pager ls` are how it
// becomes visible.
package hookio

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"github.com/unghee/pager/internal/clock"
	"github.com/unghee/pager/internal/deliver"
	"github.com/unghee/pager/internal/sessionref"
	"github.com/unghee/pager/internal/store"
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
}

// output is the object both hosts accept.
type output struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
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
	if err := st.RecordSession(ctx, rec); err != nil {
		return
	}

	// A user prompt ends whatever exchange preceded it, which is what keeps
	// ordinary work from accumulating causal depth. See startsNewTurn for what
	// "a user prompt" is allowed to mean.
	if startsNewTurn(event, in) {
		if err := deliver.ResetCausal(ctx, st, in.SessionID); err != nil {
			return
		}
	}

	text := collect(ctx, st, in.SessionID, rec.Root, rec.Tool, event)
	if text.body != "" {
		if err := write(stdout, event, text.body); err != nil {
			return
		}
	}
	if text.token != "" {
		// Best-effort: the messages have already been written out, and an
		// unconfirmed claim is retried rather than lost.
		_, _ = deliver.ConfirmDelivery(ctx, st, in.SessionID, text.token)
	}

	// Housekeeping rides along on whichever hook wins the gate.
	_, _ = st.PruneOpportunistic(ctx, store.DefaultRetention)
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
func collect(ctx context.Context, st *store.Store, session, root, tool, event string) injection {
	var out injection

	batch, err := deliver.CollectBatch(ctx, st, session, deliver.LimitsFromEnv())
	if err == nil && !batch.Empty() {
		out.body = deliver.Render(batch)
		out.token = batch.Token
	}

	if event != EventUserPromptSubmit && event != EventSessionStart {
		return out
	}
	if root == "" || tool == "" {
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
// The test is structural: the event is UserPromptSubmit and the payload carries
// a non-empty prompt. An automatic continuation — a host resuming itself after
// a Stop hook wrote context — carries no prompt text of its own, so it does not
// reset the chain and a reply it produces still counts as caused.
//
// This is the weakest link in the ping-pong defence. If a host ever synthesised
// a non-empty prompt when resuming itself, chains would reset one step early
// and depth would stop accumulating. The breaker, which counts rather than
// reasons, is what still holds in that case.
func startsNewTurn(event string, in Input) bool {
	return event == EventUserPromptSubmit && strings.TrimSpace(in.Prompt) != ""
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

func write(w io.Writer, event, context string) error {
	var out output
	out.HookSpecificOutput.HookEventName = event
	out.HookSpecificOutput.AdditionalContext = context
	enc := json.NewEncoder(w)
	return enc.Encode(out)
}

// Deadline bounds one hook invocation. A hook blocking on a wedged database
// would stall the session, which fail-open exists to prevent.
const Deadline = 5 * time.Second
