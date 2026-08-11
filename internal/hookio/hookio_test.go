package hookio

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unghee/pager/internal/clock"
	"github.com/unghee/pager/internal/deliver"
	"github.com/unghee/pager/internal/store"
)

const workspace = "/tmp/hook-workspace"

// newEnv points the binary at a throwaway database and returns a handle for
// seeding it. Nothing here may touch the real inbox.
func newEnv(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "msg.db")
	t.Setenv("PAGER_DB", path)

	st, err := store.Open(t.Context(), path, clock.System{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedInbox(t *testing.T, st *store.Store, session, tool, alias string) {
	t.Helper()
	if err := st.RecordSession(t.Context(), store.SessionRecord{
		ID: session, Tool: tool, Root: workspace,
	}); err != nil {
		t.Fatalf("record session: %v", err)
	}
	ok, err := deliver.SetAlias(t.Context(), st, alias, session)
	if err != nil || !ok {
		t.Fatalf("set alias: ok=%v err=%v", ok, err)
	}
}

func queue(t *testing.T, st *store.Store, alias, sender, body string) int64 {
	t.Helper()
	res, err := st.DB().ExecContext(t.Context(), `
		INSERT INTO messages(alias, sender_label, body, hop, origin, created_at)
		VALUES (?, ?, ?, 0, 'human', ?)`, alias, sender, body, st.Now())
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func run(t *testing.T, event, payload string) string {
	t.Helper()
	var out bytes.Buffer
	Run(context.Background(), event, strings.NewReader(payload), &out)
	return out.String()
}

// additionalContext extracts the injected text, failing if the envelope is not
// the shape both hosts expect.
func additionalContext(t *testing.T, raw string) string {
	t.Helper()
	if raw == "" {
		return ""
	}
	var parsed struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, raw)
	}
	if parsed.HookSpecificOutput.HookEventName == "" {
		t.Errorf("output has no hookEventName:\n%s", raw)
	}
	return parsed.HookSpecificOutput.AdditionalContext
}

func claudePayload(session, prompt string) string {
	return fmt.Sprintf(`{"session_id":%q,"transcript_path":"/tmp/t.jsonl","cwd":%q,`+
		`"hook_event_name":"UserPromptSubmit","prompt":%q}`, session, workspace, prompt)
}

func codexPayload(session, prompt string) string {
	return fmt.Sprintf(`{"session_id":%q,"project_dir":%q,`+
		`"hook_event_name":"UserPromptSubmit","prompt":%q}`, session, workspace, prompt)
}

// --- golden inputs -----------------------------------------------------

func TestGoldenClaudeUserPromptSubmit(t *testing.T) {
	st := newEnv(t)
	seedInbox(t, st, "claude-sess", "claude", "inbox")
	id := queue(t, st, "inbox", "@codex", "the migration is ready")

	got := additionalContext(t, run(t, EventUserPromptSubmit, claudePayload("claude-sess", "what's next?")))
	for _, want := range []string{"@codex", "the migration is ready", fmt.Sprintf("#%d", id)} {
		if !strings.Contains(got, want) {
			t.Errorf("injected context is missing %q:\n%s", want, got)
		}
	}
}

func TestGoldenCodexUserPromptSubmit(t *testing.T) {
	st := newEnv(t)
	seedInbox(t, st, "codex-sess", "codex", "inbox")
	queue(t, st, "inbox", "@claude", "review please")

	got := additionalContext(t, run(t, EventUserPromptSubmit, codexPayload("codex-sess", "다음 작업")))
	if !strings.Contains(got, "review please") {
		t.Errorf("injected context is missing the message:\n%s", got)
	}
}

// TestCrossToolDelivery is the whole point of the project: a message left by a
// Codex session reaches a Claude session, in a different workspace, unchanged.
func TestCrossToolDelivery(t *testing.T) {
	st := newEnv(t)
	ctx := t.Context()

	seedInbox(t, st, "codex-sess", "codex", "codex-inbox")
	if err := st.RecordSession(ctx, store.SessionRecord{
		ID: "claude-sess", Tool: "claude", Root: "/tmp/another-repo",
	}); err != nil {
		t.Fatalf("record claude session: %v", err)
	}
	if ok, err := deliver.SetAlias(ctx, st, "claude-inbox", "claude-sess"); err != nil || !ok {
		t.Fatalf("alias: ok=%v err=%v", ok, err)
	}

	if _, err := deliver.Send(ctx, st, deliver.SendRequest{
		Alias: "claude-inbox", Body: "handing over the parser work", Sender: "codex-sess", Label: "@codex",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := additionalContext(t, run(t, EventUserPromptSubmit, claudePayload("claude-sess", "status?")))
	if !strings.Contains(got, "handing over the parser work") {
		t.Errorf("the cross-tool message did not arrive:\n%s", got)
	}
}

// --- fail-open ---------------------------------------------------------

func TestFailsOpenOnBadInput(t *testing.T) {
	newEnv(t)
	for _, payload := range []string{
		`{"session_id":`, // truncated
		`not json at all`,
		``,
		`[]`,
		`{"session_id":""}`, // nothing to attribute to
		`null`,
	} {
		if got := run(t, EventUserPromptSubmit, payload); got != "" {
			t.Errorf("payload %q produced output %q, want silence", payload, got)
		}
	}
}

// TestFailsOpenOnUnusableDatabase: a database that cannot be opened must cost
// the session nothing.
func TestFailsOpenOnUnusableDatabase(t *testing.T) {
	dir := t.TempDir()
	// A directory where the database file should be makes Open fail.
	t.Setenv("PAGER_DB", dir)

	if got := run(t, EventUserPromptSubmit, claudePayload("s1", "hello")); got != "" {
		t.Errorf("output = %q, want silence when the database is unusable", got)
	}
}

func TestSilentWhenNothingToSay(t *testing.T) {
	st := newEnv(t)
	seedInbox(t, st, "s1", "claude", "inbox")
	if got := run(t, EventUserPromptSubmit, claudePayload("s1", "hello")); got != "" {
		t.Errorf("output = %q, want silence with no messages and no orphans", got)
	}
}

// --- causal reset ------------------------------------------------------

func causalIsClear(t *testing.T, st *store.Store, session string) bool {
	t.Helper()
	var id sql.NullInt64
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT last_inbound_id FROM sessions WHERE session_id = ?", session).Scan(&id); err != nil {
		t.Fatalf("read causal state: %v", err)
	}
	return !id.Valid
}

func TestRealPromptResetsTheChain(t *testing.T) {
	st := newEnv(t)
	ctx := t.Context()
	seedInbox(t, st, "s1", "claude", "inbox")
	queue(t, st, "inbox", "@a", "first")

	// Deliver once so the session is holding a causal representative.
	run(t, EventUserPromptSubmit, claudePayload("s1", "go"))
	if causalIsClear(t, st, "s1") {
		t.Fatal("delivery did not establish a causal representative")
	}

	// A later prompt with real text ends that exchange.
	queue(t, st, "inbox", "@a", "second")
	_ = ctx
	run(t, EventUserPromptSubmit, claudePayload("s1", "actually, do this instead"))

	// The reset happens before the new delivery, so the new message is now the
	// representative — what matters is that the epoch advanced.
	var epoch int64
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT causal_epoch FROM sessions WHERE session_id = ?", "s1").Scan(&epoch); err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if epoch != 2 {
		t.Errorf("causal_epoch = %d, want 2 after two real prompts", epoch)
	}
}

// TestEventNameIsCaseInsensitive covers the registration mistake that costs the
// most to diagnose: other tools spell these events in lowercase, and matching
// exactly would keep delivering while quietly dropping the causal reset — so
// depth accumulates and sends start failing days later for no visible reason.
func TestEventNameIsCaseInsensitive(t *testing.T) {
	for _, spelling := range []string{"userpromptsubmit", "UserPromptSubmit", "USERPROMPTSUBMIT"} {
		t.Run(spelling, func(t *testing.T) {
			st := newEnv(t)
			seedInbox(t, st, "s1", "claude", "inbox")

			run(t, spelling, claudePayload("s1", "a real prompt"))

			var epoch int64
			if err := st.DB().QueryRowContext(t.Context(),
				"SELECT causal_epoch FROM sessions WHERE session_id = ?", "s1").Scan(&epoch); err != nil {
				t.Fatalf("read epoch: %v", err)
			}
			if epoch != 1 {
				t.Errorf("causal_epoch = %d after %q, want 1 — the reset did not fire", epoch, spelling)
			}
		})
	}
}

// TestEmittedEventNameIsCanonical: whatever spelling the registration used, the
// host has to receive the one it recognises.
func TestEmittedEventNameIsCanonical(t *testing.T) {
	st := newEnv(t)
	seedInbox(t, st, "s1", "claude", "inbox")
	queue(t, st, "inbox", "@a", "hello")

	raw := run(t, "userpromptsubmit", claudePayload("s1", "go"))
	var parsed struct {
		HookSpecificOutput struct {
			HookEventName string `json:"hookEventName"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, raw)
	}
	if got := parsed.HookSpecificOutput.HookEventName; got != EventUserPromptSubmit {
		t.Errorf("hookEventName = %q, want %q", got, EventUserPromptSubmit)
	}
}

// TestOrphanHintGatingIsCaseInsensitive: the other behaviour keyed on the event
// name. Lowercase Stop must still suppress the hint.
func TestOrphanHintGatingIsCaseInsensitive(t *testing.T) {
	st := newEnv(t)
	orphanWorkspace(t, st)
	seedInbox(t, st, "fresh", "claude", "fresh-inbox")

	payload := fmt.Sprintf(`{"session_id":"fresh","cwd":%q,"hook_event_name":"Stop"}`, workspace)
	if got := run(t, "stop", payload); got != "" {
		t.Errorf("lowercase stop emitted %q, want the hint suppressed", got)
	}
}

func TestPromptlessEventDoesNotReset(t *testing.T) {
	st := newEnv(t)
	seedInbox(t, st, "s1", "claude", "inbox")

	// An automatic continuation carries no prompt of its own.
	run(t, EventUserPromptSubmit, claudePayload("s1", "   "))

	var epoch int64
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT causal_epoch FROM sessions WHERE session_id = ?", "s1").Scan(&epoch); err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if epoch != 0 {
		t.Errorf("causal_epoch = %d, want 0 — a promptless event must not reset", epoch)
	}
}

// --- delivery bookkeeping ----------------------------------------------

func TestConfirmsSoNothingIsDeliveredTwice(t *testing.T) {
	st := newEnv(t)
	seedInbox(t, st, "s1", "claude", "inbox")
	queue(t, st, "inbox", "@a", "only once")

	if got := additionalContext(t, run(t, EventUserPromptSubmit, claudePayload("s1", "go"))); !strings.Contains(got, "only once") {
		t.Fatalf("first run did not deliver:\n%s", got)
	}
	if got := run(t, EventUserPromptSubmit, claudePayload("s1", "go again")); got != "" {
		t.Errorf("second run delivered again: %q", got)
	}
}

// --- orphan hint gating ------------------------------------------------

// orphanWorkspace leaves an inbox with mail behind and no live holder.
func orphanWorkspace(t *testing.T, st *store.Store) {
	t.Helper()
	seedInbox(t, st, "departed", "claude", "left-behind")
	queue(t, st, "left-behind", "@a", "still waiting")
	// Backdate the holder's heartbeat past the staleness threshold.
	if _, err := st.DB().ExecContext(t.Context(),
		"UPDATE sessions SET heartbeat_at = ? WHERE session_id = 'departed'",
		st.Now()-store.DefaultStale.Milliseconds()-1000); err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}
}

func TestOrphanHintOnPromptOnlyOnce(t *testing.T) {
	st := newEnv(t)
	orphanWorkspace(t, st)
	seedInbox(t, st, "fresh", "claude", "fresh-inbox")

	first := additionalContext(t, run(t, EventUserPromptSubmit, claudePayload("fresh", "hello")))
	if !strings.Contains(first, "pager claim left-behind") {
		t.Fatalf("the orphan hint was not shown:\n%s", first)
	}
	if second := run(t, EventUserPromptSubmit, claudePayload("fresh", "hello again")); second != "" {
		t.Errorf("the hint was shown twice: %q", second)
	}
}

// TestStopEventStaysSilentOnOrphansOnly: Stop output continues the
// conversation, so a hint with no message would buy an agent turn that
// delivers nothing.
func TestStopEventStaysSilentOnOrphansOnly(t *testing.T) {
	st := newEnv(t)
	orphanWorkspace(t, st)
	seedInbox(t, st, "fresh", "claude", "fresh-inbox")

	payload := fmt.Sprintf(`{"session_id":"fresh","cwd":%q,"hook_event_name":"Stop"}`, workspace)
	if got := run(t, EventStop, payload); got != "" {
		t.Errorf("Stop emitted %q, want silence when only an orphan hint applies", got)
	}
}

// TestStopEventStillDeliversMessages: the suppression is of the hint, not of
// delivery.
func TestStopEventStillDeliversMessages(t *testing.T) {
	st := newEnv(t)
	seedInbox(t, st, "s1", "claude", "inbox")
	queue(t, st, "inbox", "@a", "urgent")

	payload := fmt.Sprintf(`{"session_id":"s1","cwd":%q,"hook_event_name":"Stop"}`, workspace)
	if got := additionalContext(t, run(t, EventStop, payload)); !strings.Contains(got, "urgent") {
		t.Errorf("Stop did not deliver a real message:\n%s", got)
	}
}

// TestSessionStartShowsOrphanHint covers the other event allowed to carry it.
func TestSessionStartShowsOrphanHint(t *testing.T) {
	st := newEnv(t)
	orphanWorkspace(t, st)
	seedInbox(t, st, "fresh", "claude", "fresh-inbox")

	payload := fmt.Sprintf(`{"session_id":"fresh","cwd":%q,"hook_event_name":"SessionStart"}`, workspace)
	if got := additionalContext(t, run(t, EventSessionStart, payload)); !strings.Contains(got, "pager claim left-behind") {
		t.Errorf("SessionStart did not carry the hint:\n%s", got)
	}
}

// --- session recording -------------------------------------------------

// TestRecordsSessionSoItBecomesAddressable: a session nobody has paged yet
// still has to appear, or it could never be given an alias.
func TestRecordsSessionSoItBecomesAddressable(t *testing.T) {
	st := newEnv(t)
	run(t, EventUserPromptSubmit, claudePayload("brand-new", "hi"))

	var root string
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT root FROM sessions WHERE session_id = ?", "brand-new").Scan(&root); err != nil {
		t.Fatalf("the session was not recorded: %v", err)
	}
	if root != workspace {
		t.Errorf("root = %q, want %q from the payload", root, workspace)
	}
}
