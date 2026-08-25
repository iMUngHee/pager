package hookio

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
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

// --- automatic names ---------------------------------------------------

// autoName matches the four-letter consonant-vowel shape the generator emits.
var autoName = regexp.MustCompile(`\b[bcdfghjklmnprstvwz][aeiou][bcdfghjklmnprstvwz][aeiou]\b`)

// detectionOff pins host detection to "no host".
//
// Whether detection succeeds inside a test depends on what the test binary
// happens to be a descendant of: run from a shell under a coding agent it
// finds one and records that tool, run from CI it finds nothing. A rule about
// what happens when the tool is unknown cannot be left to that. An
// unrecognised PAGER_CLIENT means no host rather than any host, which is the
// documented way to say "deliberately outside a session".
func detectionOff(t *testing.T) {
	t.Helper()
	t.Setenv("PAGER_CLIENT", "none")
}

// seedSessionOnly records a session with a known workspace and no alias, the
// way a successful detection or a pager attach would have left it.
//
// Combined with detectionOff this is also the case that decides where
// eligibility is read from: the incoming hook carries an empty tool, and the
// session qualifies for a name only because the stored row still knows one.
func seedSessionOnly(t *testing.T, st *store.Store, session, tool string) {
	t.Helper()
	if err := st.RecordSession(t.Context(), store.SessionRecord{
		ID: session, Tool: tool, Root: workspace,
	}); err != nil {
		t.Fatalf("record session: %v", err)
	}
}

func primaryAlias(t *testing.T, st *store.Store, session string) string {
	t.Helper()
	alias, err := deliver.PrimaryAlias(t.Context(), st, session)
	if err != nil {
		t.Fatalf("PrimaryAlias: %v", err)
	}
	return alias
}

// TestHookAssignsName is the fix for what opened this work: a session that
// never ran `pager alias` was identifiable only by its UUID.
func TestHookAssignsName(t *testing.T) {
	detectionOff(t)
	st := newEnv(t)
	seedSessionOnly(t, st, "s1", "claude")

	got := additionalContext(t, run(t, EventUserPromptSubmit, claudePayload("s1", "what's next?")))

	name := primaryAlias(t, st, "s1")
	if !autoName.MatchString(name) {
		t.Fatalf("session name = %q, want a four-letter automatic name", name)
	}
	if !strings.Contains(got, name) {
		t.Errorf("the session was not told its own name %q:\n%s", name, got)
	}
}

// TestHookAssignsNameAllEvents pins that naming does not depend on
// SessionStart. The registration matrix calls that event recommended and only
// UserPromptSubmit required, so a setup without it must still produce names.
func TestHookAssignsNameAllEvents(t *testing.T) {
	for _, event := range []string{EventUserPromptSubmit, EventSessionStart, EventStop, EventSubagentStop} {
		t.Run(event, func(t *testing.T) {
			detectionOff(t)
			st := newEnv(t)
			seedSessionOnly(t, st, "s1", "claude")

			run(t, event, claudePayload("s1", "hello"))

			if name := primaryAlias(t, st, "s1"); !autoName.MatchString(name) {
				t.Errorf("after a %s hook the session name = %q, want an automatic name", event, name)
			}
		})
	}
}

// TestHookIntroducesNameAtMostOnce fixes the contract as it actually is: the
// event that assigns the name says so, and no later event repeats it.
func TestHookIntroducesNameAtMostOnce(t *testing.T) {
	detectionOff(t)
	st := newEnv(t)
	seedSessionOnly(t, st, "s1", "claude")

	first := additionalContext(t, run(t, EventUserPromptSubmit, claudePayload("s1", "one")))
	name := primaryAlias(t, st, "s1")
	if !strings.Contains(first, name) {
		t.Fatalf("the assigning hook did not introduce %q:\n%s", name, first)
	}

	second := additionalContext(t, run(t, EventUserPromptSubmit, claudePayload("s1", "two")))
	if strings.Contains(second, name) {
		t.Errorf("the name was introduced again on a later hook:\n%s", second)
	}
}

// TestHookLeavesUnknownWorkspaceUnnamed is the other half of the rule. An alias
// copies root and tool when it is created and nothing updates them afterwards,
// so a session named before its host is known would carry an empty workspace
// for good — invisible to orphan discovery, untransferable by claim.
func TestHookLeavesUnknownWorkspaceUnnamed(t *testing.T) {
	detectionOff(t)
	st := newEnv(t)

	run(t, EventUserPromptSubmit, claudePayload("undetected", "hello"))

	if name := primaryAlias(t, st, "undetected"); name != "" {
		t.Errorf("session name = %q, want none while the host tool is unknown", name)
	}
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
	detectionOff(t)
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

// TestPokeDoesNotResetCausal covers the case wake made reachable. A poke is
// delivered through the host's message-injection path, so the hook sees a
// non-empty prompt that no person typed — structurally identical to typed
// input. Before the sentinel exclusion existed this reset the chain: measured
// against a live session, one poke advanced the causal epoch and cleared the
// inbound pointer, which would have billed the reply it prompted as
// human-origin instead of caused.
//
// The turn must still deliver. Poking rather than carrying the body is the
// whole design, and it is worthless if the poke suppresses the delivery it
// exists to trigger.
func TestPokeDoesNotResetCausal(t *testing.T) {
	st := newEnv(t)
	seedInbox(t, st, "s1", "claude", "inbox")
	queue(t, st, "inbox", "@a", "first")

	// A real prompt ends whatever preceded it and leaves a representative.
	run(t, EventUserPromptSubmit, claudePayload("s1", "go"))
	if causalIsClear(t, st, "s1") {
		t.Fatal("delivery did not establish a causal representative")
	}

	queue(t, st, "inbox", "@a", "second")
	poke := "pager: new mail for inbox from @a. " + deliver.PokeSentinel +
		" If it is not shown with this turn, run: msg_list"
	body := additionalContext(t, run(t, EventUserPromptSubmit, claudePayload("s1", poke)))

	if !strings.Contains(body, "second") {
		t.Errorf("poke turn did not deliver the waiting message; injected = %q", body)
	}

	var epoch int64
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT causal_epoch FROM sessions WHERE session_id = ?", "s1").Scan(&epoch); err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if epoch != 1 {
		t.Errorf("causal_epoch = %d, want 1 — a poke must not end the exchange", epoch)
	}
	if causalIsClear(t, st, "s1") {
		t.Error("poke cleared the causal representative")
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

// Every test that reaches the orphan hint pins detection off, and that is
// load-bearing rather than tidiness. The fixtures seed tool="claude", and
// RecordSession keeps a stored tool only when the incoming one is empty
// (COALESCE(NULLIF(excluded.tool, ''), sessions.tool)), so a run whose ancestry
// happens to be codex overwrites the seeded value with "codex" and the alias
// stops matching its own workspace. Pinning detection off is what makes the
// stored pair — the thing the hint is actually judged on — identical on a
// laptop, under either host, and in CI.
//
// It is also what makes the two tests that assert silence mean anything: with a
// tool stored, silence can only come from the event gate they are about, not
// from a workspace pair that never resolved.

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
	detectionOff(t)
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
	detectionOff(t)
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

// TestOrphanHintSurvivesFailedDetection is the regression test for the reason
// any of this changed: eligibility is read from the stored session row, not from
// whatever the hook's own detection managed to see.
//
// The session here never gets a tool from detection — it has one only because
// something recorded it earlier, which is the state a real session is in every
// time the ancestry walk misses. Before the fix the hint was skipped for the
// whole of such an invocation while the naming path, judging the same question
// from the stored row, carried on working.
func TestOrphanHintSurvivesFailedDetection(t *testing.T) {
	detectionOff(t)
	st := newEnv(t)
	orphanWorkspace(t, st)
	seedSessionOnly(t, st, "fresh", "claude")

	got := additionalContext(t, run(t, EventUserPromptSubmit, claudePayload("fresh", "hello")))
	if !strings.Contains(got, "pager claim left-behind") {
		t.Errorf("the hint was skipped even though the stored row knows the workspace:\n%s", got)
	}
}

// TestSessionStartShowsOrphanHint covers the other event allowed to carry it.
func TestSessionStartShowsOrphanHint(t *testing.T) {
	detectionOff(t)
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
//
// Detection is pinned off because every hook run now also tries to name the
// session, and whether that succeeds otherwise depends on what this test binary
// happens to be a descendant of. The recording this test is about must not vary
// with that.
func TestRecordsSessionSoItBecomesAddressable(t *testing.T) {
	detectionOff(t)
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
