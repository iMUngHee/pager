package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/unghee/pager/internal/clock"
	"github.com/unghee/pager/internal/store"
)

// These drive the real command dispatch end to end. They are the check that the
// pieces compose: a message left by one tool's session, in one repository,
// reaches another tool's session in a different repository.

// capture runs a command and returns its stdout.
func capture(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	go func() {
		_, _ = io.WriteString(inW, stdin)
		inW.Close()
	}()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = origIn, origOut }()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(outR)
		done <- string(b)
	}()

	runErr := run(args)
	outW.Close()
	inR.Close()
	return <-done, runErr
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := capture(t, "", args...)
	if err != nil {
		t.Fatalf("pager %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// TestE2ECrossWorkspaceDelivery is the product in one test: two repositories,
// two tools, no shared state between the projects themselves.
func TestE2ECrossWorkspaceDelivery(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	repoA := filepath.Join(dir, "repo-a")
	repoB := filepath.Join(dir, "repo-b")
	for _, d := range []string{repoA, repoB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// A Codex session in one repository.
	mustRun(t, "attach", "--session", "codex-1", "--tool", "codex", "--root", repoA)
	mustRun(t, "alias", "--session", "codex-1", "codex-box")

	// A Claude session in another.
	mustRun(t, "attach", "--session", "claude-1", "--tool", "claude", "--root", repoB)
	mustRun(t, "alias", "--session", "claude-1", "claude-box")

	sent := mustRun(t, "send", "--session", "codex-1", "claude-box", "the parser work is yours")
	if !strings.Contains(sent, "claude-box") {
		t.Fatalf("send did not confirm the target:\n%s", sent)
	}

	// The recipient's hook is what actually delivers it.
	payload := `{"session_id":"claude-1","cwd":"` + repoB + `","hook_event_name":"UserPromptSubmit","prompt":"what's next?"}`
	out, err := capture(t, payload, "hook", "UserPromptSubmit")
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if !strings.Contains(out, "the parser work is yours") {
		t.Fatalf("the message did not arrive:\n%s", out)
	}
	if !strings.Contains(out, "additionalContext") {
		t.Errorf("output is not the hook envelope:\n%s", out)
	}

	// And it is not delivered twice.
	again, err := capture(t, payload, "hook", "UserPromptSubmit")
	if err != nil {
		t.Fatalf("second hook: %v", err)
	}
	if again != "" {
		t.Errorf("the message was delivered again: %s", again)
	}

	// Neither repository holds any pager state; everything lives in the one
	// database. This is what "project-agnostic" has to mean in practice.
	for _, d := range []string{repoA, repoB} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatalf("read %s: %v", d, err)
		}
		if len(entries) != 0 {
			t.Errorf("%s gained %d entries, want none", d, len(entries))
		}
	}
}

// introducedName pulls the name a hook told a session it had been given. The
// quotes arrive escaped because the line is read out of the JSON envelope the
// host receives, which is the same text the session itself sees.
var introducedName = regexp.MustCompile(`addressable as \\?"([a-z]+)`)

func nameFromHook(t *testing.T, out string) string {
	t.Helper()
	m := introducedName.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the hook did not introduce a name:\n%s", out)
	}
	return m[1]
}

// TestE2EAutoNameDelivery is the architectural bet in one test: an automatic
// name is an ordinary alias row, so everything already built on aliases —
// addressing, the delivery join, the sender label — works through it with no
// second code path.
//
// Neither session ever runs `pager alias`. That is the situation that opened
// this work: a live round trip where the recipient saw a raw UUID as the sender
// and had no way to answer it.
func TestE2EAutoNameDelivery(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))
	// Stand outside any host, so nothing here depends on what this test binary
	// happens to be a descendant of.
	t.Setenv("PAGER_CLIENT", "none")

	// Record both sessions the way an earlier hook with a working host
	// detection would have left them: known workspace, no name yet. Going
	// through `attach` instead would name them itself, and then this test
	// would no longer be about the hook naming anything.
	st := openDB(t, filepath.Join(dir, "msg.db"))
	for _, s := range []struct{ id, tool string }{{"sender-1", "codex"}, {"recv-1", "claude"}} {
		if err := st.RecordSession(t.Context(), store.SessionRecord{ID: s.id, Tool: s.tool, Root: dir}); err != nil {
			t.Fatalf("record %s: %v", s.id, err)
		}
	}

	hook := func(session string) string {
		t.Helper()
		payload := `{"session_id":"` + session + `","cwd":"` + dir + `","hook_event_name":"UserPromptSubmit","prompt":"go"}`
		out, err := capture(t, payload, "hook", "UserPromptSubmit")
		if err != nil {
			t.Fatalf("hook for %s: %v", session, err)
		}
		return out
	}

	senderName := nameFromHook(t, hook("sender-1"))
	recvName := nameFromHook(t, hook("recv-1"))
	if senderName == recvName {
		t.Fatalf("both sessions were given the same name %q", senderName)
	}

	sent := mustRun(t, "send", "--session", "sender-1", recvName, "the parser work is yours")
	if !strings.Contains(sent, recvName) {
		t.Fatalf("send did not confirm the automatic target:\n%s", sent)
	}

	delivered := hook("recv-1")
	if !strings.Contains(delivered, "the parser work is yours") {
		t.Fatalf("the message did not arrive at the automatic name:\n%s", delivered)
	}
	// The symptom this fixes: the recipient sees who sent it.
	if !strings.Contains(delivered, senderName) {
		t.Errorf("the sender was not shown as %q:\n%s", senderName, delivered)
	}
	if strings.Contains(delivered, "sender-1") {
		t.Errorf("the recipient was shown the raw session id:\n%s", delivered)
	}
}

// reportedName pulls the name out of a command's "name:" line, rather than
// scanning the whole output for something name-shaped — a temporary directory
// in the same output could match that by chance.
var reportedName = regexp.MustCompile(`(?m)^name:\s+([a-z]+)\s*$`)

func nameFromOutput(t *testing.T, out string) string {
	t.Helper()
	m := reportedName.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no name was reported:\n%s", out)
	}
	return m[1]
}

// openDB gives a test its own handle on the same database the commands use.
func openDB(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), path, clock.System{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestAttachAssignsName covers the path that has no hooks at all. Setting a
// session up by hand is documented, and without this it would stay nameless
// forever — its messages arriving signed with a session id.
func TestAttachAssignsName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))
	t.Setenv("PAGER_CLIENT", "none")

	out := mustRun(t, "attach", "--session", "s1", "--tool", "claude", "--root", dir)
	name := nameFromOutput(t, out)

	// The name is real: it addresses the session.
	if sent := mustRun(t, "send", "--human", name, "hello"); !strings.Contains(sent, name) {
		t.Errorf("the name attach reported does not address the session:\n%s", sent)
	}
}

func TestWhoamiShowsName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))
	t.Setenv("PAGER_CLIENT", "none")

	attached := mustRun(t, "attach", "--session", "s1", "--tool", "claude", "--root", dir)
	name := nameFromOutput(t, attached)

	out := mustRun(t, "whoami", "--session", "s1")
	if !strings.Contains(out, "name:") {
		t.Errorf("whoami has no name line:\n%s", out)
	}
	if !strings.Contains(out, name) {
		t.Errorf("whoami does not show the session's name %q:\n%s", name, out)
	}
}

// TestWhoPrintsTheColumnContract pins the header and each field's position.
//
// TestWhoListsFreshNamedSessions substring-matches, so it would stay green
// through a column reorder or a renamed header — and `pager who`'s columns are a
// contract now that HOST is among them: adding a column is what broke the tmux
// badge's reading of `pager inbox`, which parses with bash `read`.
func TestWhoPrintsTheColumnContract(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msg.db")
	t.Setenv("PAGER_DB", dbPath)
	t.Setenv("PAGER_CLIENT", "none")

	name := nameFromOutput(t, mustRun(t, "attach", "--session", "s1", "--tool", "claude", "--root", dir))

	out := mustRun(t, "who")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("who printed no rows:\n%s", out)
	}
	if got := strings.Fields(lines[0]); !slices.Equal(got, []string{"NAME", "TOOL", "ROOT", "HOST", "LAST"}) {
		t.Errorf("header = %v, want NAME TOOL ROOT HOST LAST", got)
	}
	// LAST renders as two words ("just now", "5m ago"), so the row is read by
	// position from the left rather than by field count.
	row := strings.Fields(lines[1])
	if len(row) < 5 {
		t.Fatalf("row has %d fields, want at least 5: %q", len(row), lines[1])
	}
	for i, want := range []string{name, "claude", dir} {
		if row[i] != want {
			t.Errorf("field %d = %q, want %q (row: %q)", i, row[i], want, lines[1])
		}
	}
	// No host was detected for an attached session, and "unknown" is not a soft
	// "gone": it must not be reported as an abandoned inbox.
	if row[3] != "unknown" {
		t.Errorf("HOST = %q for a session with no recorded host, want unknown", row[3])
	}
}

// TestWhoMarksAGoneHost is the CLI half of the verdict msg_roster gives.
//
// Membership is decided by the heartbeat, which a session that died minutes ago
// still satisfies for twelve hours, so without HOST the roster answers a
// different question from the one it is read for.
func TestWhoMarksAGoneHost(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msg.db")
	t.Setenv("PAGER_DB", dbPath)
	t.Setenv("PAGER_CLIENT", "none")

	name := nameFromOutput(t, mustRun(t, "attach", "--session", "dead", "--tool", "claude", "--root", dir))

	// attach could not bind this: detection is pinned off, so the session
	// carries no host at all, which is the "cannot tell" case rather than this
	// one.
	st := openDB(t, dbPath)
	if _, err := st.DB().ExecContext(t.Context(),
		"UPDATE sessions SET host_client = 'claude', host_pid = ?, host_start = ? WHERE session_id = ?",
		deadPid(t), 1, "dead"); err != nil {
		t.Fatalf("bind a dead host: %v", err)
	}

	out := mustRun(t, "who")
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, name) {
			continue
		}
		if !strings.Contains(line, "gone") {
			t.Errorf("who does not mark a session whose host has exited:\n%s", line)
		}
		return
	}
	t.Errorf("who does not list the session at all:\n%s", out)
}

// TestWhoListsFreshNamedSessions: the roster answers "who can I page", so a
// session whose hooks stopped running hours ago has no business in it.
func TestWhoListsFreshNamedSessions(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msg.db")
	t.Setenv("PAGER_DB", dbPath)
	t.Setenv("PAGER_CLIENT", "none")

	fresh := nameFromOutput(t, mustRun(t, "attach", "--session", "fresh", "--tool", "claude", "--root", dir))
	gone := nameFromOutput(t, mustRun(t, "attach", "--session", "gone", "--tool", "codex", "--root", dir))
	if fresh == gone {
		t.Fatalf("both sessions were named %q", fresh)
	}

	// Age one session out. The commands run on the real clock, so staleness
	// has to be written rather than waited for.
	st := openDB(t, dbPath)
	if _, err := st.DB().ExecContext(t.Context(),
		"UPDATE sessions SET heartbeat_at = ? WHERE session_id = ?",
		st.Now()-(store.DefaultStale+time.Hour).Milliseconds(), "gone"); err != nil {
		t.Fatalf("age the session: %v", err)
	}

	out := mustRun(t, "who")
	if !strings.Contains(out, fresh) {
		t.Errorf("the roster is missing the live session %q:\n%s", fresh, out)
	}
	if strings.Contains(out, gone) {
		t.Errorf("the roster still lists the stale session %q:\n%s", gone, out)
	}
	if !strings.Contains(out, "claude") || !strings.Contains(out, dir) {
		t.Errorf("the roster does not say where the session is:\n%s", out)
	}
}

// TestSchemaVersionUnchanged: migrate is the only thing that writes
// user_version, so ordinary commands must leave it where they found it.
//
// It used to assert the literal 1, meaning "automatic names needed no schema
// change". That reading died when migrate learned to upgrade an existing
// database: pager now migrates one on open, by design, and a literal here would
// have to be edited on every schema change. What is left is the weaker but
// durable invariant — attach, a hook and who write no version of their own. The
// version itself is owned by internal/store, where schemaVersion is visible.
func TestSchemaVersionUnchanged(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msg.db")
	t.Setenv("PAGER_DB", dbPath)
	t.Setenv("PAGER_CLIENT", "none")

	mustRun(t, "attach", "--session", "s1", "--tool", "claude", "--root", dir)
	baseline := schemaVersionOf(t, dbPath)

	payload := `{"session_id":"s1","cwd":"` + dir + `","hook_event_name":"UserPromptSubmit","prompt":"go"}`
	if _, err := capture(t, payload, "hook", "UserPromptSubmit"); err != nil {
		t.Fatalf("hook: %v", err)
	}
	mustRun(t, "who")

	if got := schemaVersionOf(t, dbPath); got != baseline {
		t.Errorf("user_version = %d after ordinary commands, want the unchanged %d", got, baseline)
	}
}

// schemaVersionOf reads the version a database reports.
func schemaVersionOf(t *testing.T, dbPath string) int {
	t.Helper()
	st := openDB(t, dbPath)
	var version int
	if err := st.DB().QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return version
}

// TestE2EReplyCarriesCausalDepth: the reply to a delivered message is not a
// fresh conversation, and the CLI reports the depth it was given.
func TestE2EReplyCarriesCausalDepth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "a", "--tool", "codex", "--root", dir)
	mustRun(t, "alias", "--session", "a", "a-box")
	mustRun(t, "attach", "--session", "b", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "b", "b-box")

	mustRun(t, "send", "--session", "a", "b-box", "look at this")

	payload := `{"session_id":"b","cwd":"` + dir + `","hook_event_name":"Stop"}`
	if _, err := capture(t, payload, "hook", "Stop"); err != nil {
		t.Fatalf("hook: %v", err)
	}

	reply := mustRun(t, "send", "--session", "b", "a-box", "on it")
	if !strings.Contains(reply, "hop 1") || !strings.Contains(reply, "caused") {
		t.Errorf("the reply did not inherit causal depth:\n%s", reply)
	}
}

// TestE2ETrailingHumanFlag is the invocation the usage string has always
// advertised. Writing --human last used to leave it unparsed: it was joined
// into the body the recipient reads, and the send it was meant to mark as an
// operator's was recorded as ordinary caused traffic — charged to the automatic
// budget and carrying causal depth it should have broken.
//
// The sending session has to be holding a delivered message for this to bite.
// Without one, a send is treated as human-origin anyway and the lost flag
// changes nothing but the body.
func TestE2ETrailingHumanFlag(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msg.db")
	t.Setenv("PAGER_DB", dbPath)

	mustRun(t, "attach", "--session", "a", "--tool", "codex", "--root", dir)
	mustRun(t, "alias", "--session", "a", "a-box")
	mustRun(t, "attach", "--session", "b", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "b", "b-box")

	mustRun(t, "send", "--session", "a", "b-box", "look at this")
	payload := `{"session_id":"b","cwd":"` + dir + `","hook_event_name":"Stop"}`
	if _, err := capture(t, payload, "hook", "Stop"); err != nil {
		t.Fatalf("hook: %v", err)
	}

	out := mustRun(t, "send", "--session", "b", "a-box", "operator says hi", "--human")
	if !strings.Contains(out, "hop 0") || !strings.Contains(out, "human") {
		t.Errorf("a trailing --human did not break the causal chain:\n%s", out)
	}

	st := openDB(t, dbPath)
	var body string
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT body FROM messages ORDER BY id DESC LIMIT 1").Scan(&body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if body != "operator says hi" {
		t.Errorf("body = %q, want the flag kept out of it", body)
	}
}

// TestE2ETrailingHumanUnattributed covers the dead end the same bug created for
// anyone outside a session: the send was refused with a message naming the very
// flag they had just supplied.
func TestE2ETrailingHumanUnattributed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "b", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "b", "b-box")
	t.Setenv("PAGER_CLIENT", "none")

	out, err := capture(t, "", "send", "b-box", "hello", "--human")
	if err != nil {
		t.Fatalf("send with a trailing --human was refused: %v\n%s", err, out)
	}
}

// TestE2ETrailingSessionFlag: alias and claim declare one positional, so the
// unparsed flag and its value stayed behind as extra arguments and the command
// failed its own usage check before doing anything.
func TestE2ETrailingSessionFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))
	t.Setenv("PAGER_CLIENT", "none")

	mustRun(t, "attach", "--session", "s1", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "review-box", "--session", "s1")

	if out := mustRun(t, "whoami", "--session", "s1"); !strings.Contains(out, "review-box") {
		t.Errorf("the name set with a trailing --session did not stick:\n%s", out)
	}

	// claim reaches its own decision now rather than dying on usage. Its holder
	// is still live, so refusing is the correct answer — the point is which
	// refusal it is.
	_, err := capture(t, "", "claim", "review-box", "--session", "s1")
	if err == nil {
		t.Fatal("claim succeeded against a live holder")
	}
	if strings.Contains(err.Error(), "usage:") {
		t.Errorf("claim still fails on argument count: %v", err)
	}
}

// TestE2ESendWithoutContextIsRefused is the fail-closed default at the command
// line, and the escape hatch beside it.
func TestE2ESendWithoutContextIsRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "b", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "b", "b-box")

	// Stand outside any session, the way an independent shell does. Without
	// this the test would run inside a real host and attach would genuinely
	// have bound it, so the send would be correctly attributed.
	t.Setenv("PAGER_CLIENT", "none")

	if _, err := capture(t, "", "send", "b-box", "who am I"); err == nil {
		t.Error("an unattributed send succeeded, want it refused")
	}
	if _, err := capture(t, "", "send", "--human", "b-box", "it is me"); err != nil {
		t.Errorf("--human send: %v", err)
	}
}

// TestE2EAliasHandover walks the takeover path a person actually types.
func TestE2EAliasHandover(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "old", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "old", "shared")
	mustRun(t, "attach", "--session", "new", "--tool", "claude", "--root", dir)

	// The holder is still alive, so neither naming nor claiming may take it.
	if _, err := capture(t, "", "alias", "--session", "new", "shared"); err == nil {
		t.Error("alias took over a live session's inbox")
	}
	if _, err := capture(t, "", "claim", "--session", "new", "shared"); err == nil {
		t.Error("claim took over a live session's inbox")
	}
}

func TestE2EListAndPrune(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "b", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "b", "b-box")
	mustRun(t, "send", "--human", "b-box", "hello there")

	listed := mustRun(t, "ls", "--session", "b")
	for _, want := range []string{"b-box", "waiting", "hello there"} {
		if !strings.Contains(listed, want) {
			t.Errorf("ls output is missing %q:\n%s", want, listed)
		}
	}
	if out := mustRun(t, "ls", "--session", "b", "--expired"); !strings.Contains(out, "nothing here") {
		t.Errorf("a fresh message showed up as expired:\n%s", out)
	}

	// Deliver it, then ask the two questions that must now differ. This is the
	// cost contract the --waiting flag exists for: once an inbox has been read,
	// polling it should not carry the history back every time.
	payload := `{"session_id":"b","cwd":"` + dir + `","hook_event_name":"UserPromptSubmit","prompt":"anything?"}`
	if _, err := capture(t, payload, "hook", "UserPromptSubmit"); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if out := mustRun(t, "ls", "--session", "b"); !strings.Contains(out, "hello there") {
		t.Errorf("a delivered message vanished from the full listing:\n%s", out)
	}
	if out := mustRun(t, "ls", "--session", "b", "--waiting"); !strings.Contains(out, "nothing here") {
		t.Errorf("--waiting still rendered the delivered history:\n%s", out)
	}
	if out := mustRun(t, "prune", "--dry-run"); !strings.Contains(out, "0 message(s) would be deleted") {
		t.Errorf("dry-run prune: %s", out)
	}
}

// TestE2EPruneSaysWhenAnotherPruneHoldsTheLease is what the serialisation is
// worth to a person. A prune that yields deletes nothing, and printing "0
// message(s) deleted" for that would read as "nothing had expired" — the
// opposite of the truth, and the reason this line exists rather than the count.
func TestE2EPruneSaysWhenAnotherPruneHoldsTheLease(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msg.db")
	t.Setenv("PAGER_DB", dbPath)

	st, err := store.Open(t.Context(), dbPath, clock.System{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Stand in for a prune that is under way: the lease is taken and not yet
	// released. Writing meta directly is what lets the command run in this
	// process without a second one actually pruning.
	if _, err := st.Exec(t.Context(),
		"UPDATE meta SET lease_token = 'held', prune_started_at = ? WHERE key = 'prune'",
		st.Now()); err != nil {
		t.Fatalf("hold the lease: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	out := mustRun(t, "prune")
	if !strings.Contains(out, "another prune is already running") {
		t.Errorf("prune did not say it yielded:\n%s", out)
	}
	if strings.Contains(out, "message(s) deleted") {
		t.Errorf("prune reported a deletion count while yielding:\n%s", out)
	}
}

// TestE2EExportRoundTrip drives the command a person actually types and checks
// what lands on stdout is JSONL carrying the message they sent.
func TestE2EExportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "e", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "e", "e-box")
	mustRun(t, "send", "--human", "e-box", "keep me")

	out := mustRun(t, "export")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("export wrote %d lines, want 1:\n%s", len(lines), out)
	}

	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("export line is not valid JSON: %v\n%s", err, lines[0])
	}
	if rec["body"] != "keep me" {
		t.Errorf("exported body is %v, want %q", rec["body"], "keep me")
	}
	if rec["alias"] != "e-box" {
		t.Errorf("exported alias is %v, want %q", rec["alias"], "e-box")
	}
	// --human is a claim about who wrote the message, not a claim that no
	// session was resolvable — the sending session is still recorded when there
	// is one. What the origin does determine is the causal chain: a human send
	// starts one, so it has no cause.
	if rec["origin"] != "human" {
		t.Errorf("exported origin is %v, want %q", rec["origin"], "human")
	}
	if v, ok := rec["cause_id"]; !ok || v != nil {
		t.Errorf("exported cause_id is %v (present: %v), want an explicit null", v, ok)
	}
}

// TestE2EPruneArchivesThroughTheBinary drives a real, non-dry-run prune through
// command dispatch and checks the file actually lands beside the database.
//
// Every other archive test holds a *store.Store directly. This one goes through
// openStore and store.DefaultPath instead, which is the plumbing that decides
// where the archive ends up — a mistake there would be invisible to all of them,
// and the surrounding prune e2e only ever runs --dry-run against a message that
// is not expired, so it never reaches the archive code at all.
func TestE2EPruneArchivesThroughTheBinary(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msg.db")
	t.Setenv("PAGER_DB", dbPath)
	t.Setenv("PAGER_ARCHIVE", "")

	mustRun(t, "attach", "--session", "p", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "p", "p-box")
	mustRun(t, "send", "--human", "p-box", "archive me")

	// Age the message past retention. Retention is a constant with no override,
	// so the only way to make a real prune delete anything is to move the row.
	st, err := store.Open(t.Context(), dbPath, clock.System{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	old := st.Now() - (store.DefaultRetention + time.Hour).Milliseconds()
	if _, err := st.DB().ExecContext(t.Context(),
		"UPDATE messages SET created_at = ?, delivered_at = ?", old, old); err != nil {
		t.Fatalf("age message: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	if out := mustRun(t, "prune"); !strings.Contains(out, "1 message(s) deleted") {
		t.Fatalf("prune did not delete the expired message: %s", out)
	}

	data, err := os.ReadFile(filepath.Join(dir, "archive.jsonl"))
	if err != nil {
		t.Fatalf("archive was not written beside the database: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(string(data), "\n")), &rec); err != nil {
		t.Fatalf("archived line is not valid JSON: %v\n%s", err, data)
	}
	if rec["body"] != "archive me" {
		t.Errorf("archived body is %v, want %q", rec["body"], "archive me")
	}

	if out := mustRun(t, "export"); out != "" {
		t.Errorf("export still returns rows after the prune:\n%s", out)
	}
}

// TestE2EExportRejectsArguments: a flag that silently did nothing would be
// worse than no flag at all, because the output would look filtered.
func TestE2EExportRejectsArguments(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	out, err := capture(t, "", "export", "--since", "3d")
	if err == nil {
		t.Errorf("export accepted an unknown argument and wrote:\n%s", out)
	}
	if out != "" {
		t.Errorf("export wrote output despite failing:\n%s", out)
	}
}

// TestE2EWorksWithoutPMRoadmap: pager must not need .agents to exist.
func TestE2EWorksWithoutPMRoadmap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "solo", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "solo", "solo-box")
	mustRun(t, "send", "--human", "solo-box", "no project management here")

	payload := `{"session_id":"solo","cwd":"` + dir + `","hook_event_name":"UserPromptSubmit","prompt":"go"}`
	out, err := capture(t, payload, "hook", "UserPromptSubmit")
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if !strings.Contains(out, "no project management here") {
		t.Errorf("delivery failed without a task store:\n%s", out)
	}
}

func TestE2EUnknownAndAmbiguousTargets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "b", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "b", "review-one")
	mustRun(t, "alias", "--session", "b", "review-two")

	if _, err := capture(t, "", "send", "--human", "nobody", "hi"); err == nil {
		t.Error("a send to an unknown target succeeded")
	}
	_, err := capture(t, "", "send", "--human", "review", "hi")
	if err == nil {
		t.Fatal("an ambiguous target succeeded, want the candidates reported")
	}
	if !strings.Contains(err.Error(), "review-one") || !strings.Contains(err.Error(), "review-two") {
		t.Errorf("the error does not list the candidates: %v", err)
	}
}

// TestE2EInboxReportsEveryWaitingInbox is the whole-store question nothing used
// to answer: `ls` is scoped to one session, so learning who is waiting meant one
// call per session or reading the tables directly.
//
// PAGER_CLIENT is pinned to a non-candidate so host detection fails the same way
// on every machine. That is what makes the HOST column deterministic here — the
// test binary is itself a descendant of whatever agent ran `go test`, so an
// unpinned run would find a real live host and the expected word would depend on
// who was running the suite.
func TestE2EInboxReportsEveryWaitingInbox(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))
	t.Setenv("PAGER_CLIENT", "none")

	if out := mustRun(t, "inbox"); !strings.Contains(out, "nothing waiting") {
		t.Errorf("an empty store did not say so:\n%s", out)
	}

	for _, s := range []struct{ session, box string }{{"a", "a-box"}, {"b", "b-box"}} {
		mustRun(t, "attach", "--session", s.session, "--tool", "claude", "--root", dir)
		mustRun(t, "alias", "--session", s.session, s.box)
	}
	mustRun(t, "send", "--human", "a-box", "one for a")
	mustRun(t, "send", "--human", "a-box", "two for a")
	mustRun(t, "send", "--human", "b-box", "one for b")

	out := mustRun(t, "inbox")
	if !strings.Contains(out, "INBOX") || !strings.Contains(out, "WAITING") || !strings.Contains(out, "HOST") {
		t.Fatalf("the header is not the three documented columns:\n%s", out)
	}
	// Counts per inbox, and no cross-contamination between them.
	for _, want := range []string{"a-box", "b-box"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if got := inboxRow(t, out, "a-box"); got[1] != "2" {
		t.Errorf("a-box waiting = %q, want 2:\n%s", got[1], out)
	}
	if got := inboxRow(t, out, "b-box"); got[1] != "1" {
		t.Errorf("b-box waiting = %q, want 1:\n%s", got[1], out)
	}
	// No host was detected, which is not the same as the process being gone.
	if got := inboxRow(t, out, "a-box"); got[2] != "unknown" {
		t.Errorf("a-box host = %q, want unknown when detection never ran:\n%s", got[2], out)
	}

	// Delivering a-box's mail must take it off the listing entirely, since the
	// count means waiting and nothing is waiting there any more.
	payload := `{"session_id":"a","cwd":"` + dir + `","hook_event_name":"UserPromptSubmit","prompt":"anything?"}`
	if _, err := capture(t, payload, "hook", "UserPromptSubmit"); err != nil {
		t.Fatalf("hook: %v", err)
	}
	out = mustRun(t, "inbox")
	if strings.Contains(out, "a-box") {
		t.Errorf("a delivered inbox is still listed as waiting:\n%s", out)
	}
	if !strings.Contains(out, "b-box") {
		t.Errorf("an untouched inbox vanished:\n%s", out)
	}
}

// inboxRow returns the whitespace-separated fields of the row for alias.
func inboxRow(t *testing.T, out, alias string) []string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == alias {
			return fields
		}
	}
	t.Fatalf("no row for %q in:\n%s", alias, out)
	return nil
}

// TestE2EInboxRejectsArguments follows export: quietly listing everything in
// response to a flag that does not exist would look like the flag worked.
func TestE2EInboxRejectsArguments(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))
	t.Setenv("PAGER_CLIENT", "none")

	if _, err := capture(t, "", "inbox", "b-box"); err == nil {
		t.Error("inbox accepted a positional argument")
	}
	if _, err := capture(t, "", "inbox", "--live"); err == nil {
		t.Error("inbox accepted an undefined flag")
	}
}

// deadPid returns a pid whose process has certainly exited.
//
// Its number may be recycled later, but then the start token recorded against
// it no longer matches and the probe still answers "not the one we recorded" —
// which is the same verdict this test wants, by either route.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run throwaway process: %v", err)
	}
	return cmd.Process.Pid
}

// TestE2ESendToStrandedHolderSaysNobodyIsThere is the defect this closes.
//
// Automatic naming made the common shape different from the one the old notice
// was written for: the holder row outlives its session, so a dead inbox still
// resolves with a session id and the notice never fired. What fired instead was
// wake's "it will be delivered on its next activity" — true of a queue, false
// of a reader, and read as reassurance at exactly the moment nobody was there.
func TestE2ESendToStrandedHolderSaysNobodyIsThere(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msg.db")
	t.Setenv("PAGER_DB", dbPath)
	t.Setenv("PAGER_CLIENT", "none")
	// Wake runs for real, with HOME pointing at an empty directory so both
	// adapters look for their surfaces and find none — the same arrangement
	// TestSendSurvivesWakeFailure uses. Pinned off, the "was not poked" line
	// could never appear and the assertion below would prove nothing.
	t.Setenv("PAGER_WAKE", "on")
	t.Setenv("HOME", t.TempDir())

	mustRun(t, "attach", "--session", "gone-session", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "gone-session", "gone-box")

	// Bind the holder to a process that has exited. attach could not do this:
	// host detection is pinned off so the session carries no host at all, which
	// is the "cannot tell" case rather than this one.
	st := openDB(t, dbPath)
	if _, err := st.Exec(t.Context(),
		"UPDATE sessions SET host_client = 'claude', host_pid = ?, host_start = ? WHERE session_id = ?",
		deadPid(t), 1, "gone-session"); err != nil {
		t.Fatalf("bind a dead host: %v", err)
	}

	out := mustRun(t, "send", "--human", "gone-box", "anyone there?")
	if !strings.Contains(out, "gone-box's session is gone") {
		t.Errorf("send did not say the holder is gone:\n%s", out)
	}
	if strings.Contains(out, "was not poked") {
		t.Errorf("send still reported the poke outcome, which promises an activity "+
			"that is not coming:\n%s", out)
	}
	// The message is stored either way — the notice is about who will read it,
	// not about whether the send worked.
	if !strings.Contains(out, "sent #1 to gone-box") {
		t.Errorf("the send itself did not report success:\n%s", out)
	}
	if listed := mustRun(t, "ls", "--session", "gone-session"); !strings.Contains(listed, "anyone there?") {
		t.Errorf("the message was not stored:\n%s", listed)
	}
}

// TestE2ESendToUncheckableHolderKeepsTheOldBehaviour is the other half, and the
// one a wrong implementation breaks silently. A session with no recorded host
// has not been shown to be gone, so it must still be treated as possibly there.
func TestE2ESendToUncheckableHolderKeepsTheOldBehaviour(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))
	t.Setenv("PAGER_CLIENT", "none")
	// Wake runs for real, with HOME pointing at an empty directory so both
	// adapters look for their surfaces and find none — the same arrangement
	// TestSendSurvivesWakeFailure uses. Pinned off, the "was not poked" line
	// could never appear and the assertion below would prove nothing.
	t.Setenv("PAGER_WAKE", "on")
	t.Setenv("HOME", t.TempDir())

	mustRun(t, "attach", "--session", "unknown-session", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "unknown-session", "unknown-box")

	out := mustRun(t, "send", "--human", "unknown-box", "hello")
	if strings.Contains(out, "session is gone") {
		t.Errorf("a session whose host was never detected was reported as gone:\n%s", out)
	}
}
