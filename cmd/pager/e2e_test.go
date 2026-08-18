package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
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

func TestAgoRendersCoarsely(t *testing.T) {
	const now = int64(1_000_000_000)
	for _, tc := range []struct {
		name string
		age  time.Duration
		want string
	}{
		{"seconds", 30 * time.Second, "just now"},
		{"minutes", 5 * time.Minute, "5m ago"},
		{"hours", 3 * time.Hour, "3h ago"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ago(now, now-tc.age.Milliseconds()); got != tc.want {
				t.Errorf("ago(%s) = %q, want %q", tc.age, got, tc.want)
			}
		})
	}
}

// TestSchemaVersionUnchanged: automatic names were designed to need no schema
// change, so a database written by the previous version keeps working and this
// one must not quietly migrate it.
func TestSchemaVersionUnchanged(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msg.db")
	t.Setenv("PAGER_DB", dbPath)
	t.Setenv("PAGER_CLIENT", "none")

	mustRun(t, "attach", "--session", "s1", "--tool", "claude", "--root", dir)
	payload := `{"session_id":"s1","cwd":"` + dir + `","hook_event_name":"UserPromptSubmit","prompt":"go"}`
	if _, err := capture(t, payload, "hook", "UserPromptSubmit"); err != nil {
		t.Fatalf("hook: %v", err)
	}
	mustRun(t, "who")

	st := openDB(t, dbPath)
	var version int
	if err := st.DB().QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 1 {
		t.Errorf("user_version = %d, want the unchanged 1", version)
	}
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
	if out := mustRun(t, "prune", "--dry-run"); !strings.Contains(out, "0 message(s) would be deleted") {
		t.Errorf("dry-run prune: %s", out)
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
