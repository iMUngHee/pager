//go:build live

// Live tests run against real hosts. They are behind a build tag because the
// unit tests deliberately cannot reach what matters here: whether a poke makes
// a real session act, and whether the host puts the poke body somewhere the
// hook can recognise it.
//
// The Claude test brings its own receiving session rather than borrowing one.
// That is not tidiness — the hooks a real session runs resolve `pager` from
// PATH, so borrowing one would test the installed binary rather than this
// working tree, and installing to make the test pass would put unreviewed code
// into every session on the machine.
package wake

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/unghee/pager/internal/clock"
	"github.com/unghee/pager/internal/deliver"
	"github.com/unghee/pager/internal/store"
)

// buildPager compiles this working tree so the receiving session's hook runs
// the code under test.
func buildPager(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pager")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/unghee/pager/cmd/pager")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build pager: %v\n%s", err, out)
	}
	return bin
}

// writeSettings points a session's UserPromptSubmit hook at our binary by
// absolute path, bypassing PATH entirely.
func writeSettings(t *testing.T, bin string) string {
	t.Helper()
	settings := map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []any{map[string]any{
				"matcher": "",
				"hooks": []any{map[string]any{
					"type":    "command",
					"command": bin + " hook",
					"timeout": 10000,
				}},
			}},
		},
	}
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return path
}

func registrySnapshot(t *testing.T) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".claude", "sessions"))
	if err != nil {
		return seen
	}
	for _, e := range entries {
		seen[e.Name()] = true
	}
	return seen
}

// waitForInbox blocks until a session that was not there before registers a
// messaging socket, and returns its session id.
func waitForInbox(t *testing.T, before map[string]bool) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".claude", "sessions")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if before[e.Name()] || !claudeRecordName.MatchString(e.Name()) {
					continue
				}
				rec, err := readClaudeRecord(filepath.Join(dir, e.Name()))
				if err == nil && rec.MessagingSocketPath != "" && rec.SessionID != "" {
					return rec.SessionID
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no new session registered a messaging inbox within 30s — is the gate env set?")
	return ""
}

func causalEpoch(t *testing.T, st *store.Store, session string) int64 {
	t.Helper()
	var epoch int64
	if err := st.DB().QueryRowContext(context.Background(),
		"SELECT causal_epoch FROM sessions WHERE session_id = ?", session).Scan(&epoch); err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	return epoch
}

func deliveredAt(t *testing.T, st *store.Store, id int64) sql.NullInt64 {
	t.Helper()
	var at sql.NullInt64
	if err := st.DB().QueryRowContext(context.Background(),
		"SELECT delivered_at FROM messages WHERE id = ?", id).Scan(&at); err != nil {
		t.Fatalf("read delivered_at: %v", err)
	}
	return at
}

// TestLiveClaudeWake is the gate the plan puts before wiring: this adapter, not
// a hand-run experiment, must wake a real session.
//
// Two things are asserted together and both matter. The message must reach the
// session — proving a poke that carries no body still triggers delivery. And
// the causal epoch must not move — proving the sentinel survives the round trip
// through the host, which is the one thing a synthesised hook payload cannot
// tell us.
func TestLiveClaudeWake(t *testing.T) {
	bin := buildPager(t)
	settings := writeSettings(t, bin)
	dbPath := filepath.Join(t.TempDir(), "msg.db")

	before := registrySnapshot(t)

	// --setting-sources "" drops the user's own settings, and with them the
	// pager hook registered there. Without it two hooks run against the same
	// database: this working tree's and whatever is installed on PATH. The
	// installed one has no sentinel, so it resets the causal epoch and the
	// assertion below fails for a reason that has nothing to do with this
	// change. Measured: with user settings loaded the installed binary creates
	// its own database alongside ours; with them dropped it does not run.
	//
	// --settings still applies, and auth is untouched — isolating the whole
	// config directory instead would take the credentials with it.
	cmd := exec.Command("claude", "-p",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", "haiku", "--allowedTools", "",
		"--settings", settings, "--setting-sources", "")
	cmd.Env = append(os.Environ(), "CLAUDE_CODE_HARBOR_KITE=1", "PAGER_DB="+dbPath)
	cmd.Dir = t.TempDir()
	stdin, err := cmd.StdinPipe() // held open so the session sits idle
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start claude: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for scanner.Scan() {
		}
	}()

	target := waitForInbox(t, before)
	time.Sleep(2 * time.Second) // let it settle into idle

	ctx := context.Background()
	st, err := store.Open(ctx, dbPath, clock.System{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if err := st.RecordSession(ctx, store.SessionRecord{
		ID: target, Tool: "claude", Root: cmd.Dir,
	}); err != nil {
		t.Fatalf("record session: %v", err)
	}
	if _, err := deliver.SetAlias(ctx, st, "livetarget", target); err != nil {
		t.Fatalf("set alias: %v", err)
	}
	sent, err := deliver.Send(ctx, st, deliver.SendRequest{
		Alias: "livetarget", Body: "live probe body", Label: "livetest", Human: true,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	epochBefore := causalEpoch(t, st, target)

	got := Wake(ctx, target, PokeBody("livetarget", "livetest", ""))
	if got.Outcome != Woken {
		t.Fatalf("Wake() = %q via %q (%v), want %q", got.Outcome, got.Via, got.Err, Woken)
	}
	if got.Via != "uds" {
		t.Errorf("Via = %q, want \"uds\"", got.Via)
	}

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if deliveredAt(t, st, sent.ID).Valid {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if at := deliveredAt(t, st, sent.ID); !at.Valid {
		t.Fatal("message never reached the woken session — a poke must trigger the hook's delivery")
	}

	if after := causalEpoch(t, st, target); after != epochBefore {
		t.Errorf("causal_epoch %d -> %d; a poke must not end the recipient's exchange", epochBefore, after)
	}
}

// TestLiveCodexWake needs a thread that already exists, and poking it puts a
// message into a session someone is using. That is why it takes the thread id
// from the environment rather than picking one: writing into a real
// conversation is not something a test should decide on its own.
func TestLiveCodexWake(t *testing.T) {
	thread := os.Getenv("PAGER_LIVE_CODEX_THREAD")
	if thread == "" {
		t.Skip("set PAGER_LIVE_CODEX_THREAD to a codex thread id to run this; it writes into that session")
	}

	w := newCodexWaker()
	if !w.reach(thread) {
		t.Fatalf("reach(%s) = false — no rollout found; the thread must have run at least one turn", thread)
	}

	queued := countCodexQueue(t)
	body := PokeBody("livetarget", "livetest", "")
	if err := w.poke(context.Background(), thread, body); err != nil {
		t.Fatalf("poke: %v", err)
	}

	// The queue drains within seconds when the session is live; seeing it grow
	// and then shrink is what proves the session consumed it.
	grew := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		n := countCodexQueue(t)
		if n > queued {
			grew = true
		}
		if grew && n <= queued {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !grew {
		t.Fatal("queue never grew — codex queue reported success but nothing was enqueued")
	}
	t.Fatal("queued item was never consumed — the session did not pick it up")
}

func countCodexQueue(t *testing.T) int {
	t.Helper()
	home, _ := os.UserHomeDir()
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", filepath.Join(home, ".codex", "queue_1.sqlite")))
	if err != nil {
		t.Fatalf("open codex queue: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM queued_items").Scan(&n); err != nil {
		t.Fatalf("count queued_items: %v", err)
	}
	return n
}
