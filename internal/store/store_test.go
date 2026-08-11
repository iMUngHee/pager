package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unghee/pager/internal/clock"
)

// testBase is an arbitrary fixed instant. Tests move the fake clock relative to
// it, so nothing here depends on the wall clock.
var testBase = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) (*Store, *clock.Fake) {
	t.Helper()
	fake := clock.NewFake(testBase)
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), ".pager", "msg.db"), fake)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, fake
}

// insertDelivered adds a human-origin message that was created `ago` before the
// fake clock's current time and has already been delivered — i.e. a prune
// candidate once it ages past retention.
func insertDelivered(t *testing.T, s *Store, ago time.Duration) int64 {
	t.Helper()
	created := s.Now() - ago.Milliseconds()
	res, err := s.db.ExecContext(t.Context(), `
		INSERT INTO messages(alias, body, hop, origin, created_at, delivered_at)
		VALUES ('inbox', 'body', 0, 'human', ?, ?)`, created, created)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

func countMessages(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

func messageExists(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE id = ?", id).Scan(&n); err != nil {
		t.Fatalf("lookup message %d: %v", id, err)
	}
	return n == 1
}

// TestConnectionPragmas proves the DSN-encoded settings actually reach the
// connection. Without this, a query-encoding change could silently disable
// foreign_keys and the prune exclusion rules would stop being load-bearing.
func TestConnectionPragmas(t *testing.T) {
	s, _ := newStore(t)
	for _, tc := range []struct{ pragma, want string }{
		{"journal_mode", "wal"},
		{"busy_timeout", "5000"},
		{"synchronous", "1"}, // NORMAL
		{"foreign_keys", "1"},
	} {
		var got string
		if err := s.db.QueryRowContext(t.Context(), "PRAGMA "+tc.pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", tc.pragma, err)
		}
		if got != tc.want {
			t.Errorf("PRAGMA %s = %q, want %q", tc.pragma, got, tc.want)
		}
	}
}

func TestDBPermissions(t *testing.T) {
	s, _ := newStore(t)
	var path string
	if err := s.db.QueryRowContext(t.Context(), "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path); err != nil {
		t.Fatalf("database_list: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if got := fi.Mode().Perm(); got != filePerm {
		t.Errorf("db mode = %o, want %o", got, filePerm)
	}

	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != dirPerm {
		t.Errorf("dir mode = %o, want %o", got, dirPerm)
	}
}

// TestConcurrentMigrate opens the same fresh database from several processes'
// worth of handles at once. BEGIN EXCLUSIVE must serialise them so the DDL runs
// exactly once and every caller ends up with a usable store.
func TestConcurrentMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".pager", "msg.db")
	const openers = 8

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, openers)
	stores := make([]*Store, openers)

	for i := range openers {
		wg.Go(func() {
			<-start // barrier: force the migrations to actually overlap
			s, err := Open(context.Background(), path, clock.NewFake(testBase))
			stores[i], errs[i] = s, err
		})
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("opener %d: %v", i, err)
		}
		t.Cleanup(func() { _ = stores[i].Close() })
	}

	s := stores[0]
	var version int
	if err := s.db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}

	// The schema must exist once, not once per opener.
	for _, table := range []string{"messages", "sessions", "aliases", "meta"} {
		var n int
		if err := s.db.QueryRowContext(t.Context(),
			"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&n); err != nil {
			t.Fatalf("sqlite_master %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %s defined %d times, want 1", table, n)
		}
	}

	// Every opener must have a working handle, not just the migration winner.
	for i, st := range stores {
		if _, err := st.db.ExecContext(t.Context(),
			"INSERT INTO messages(alias, body, hop, origin, created_at) VALUES ('inbox', 'x', 0, 'human', ?)",
			st.Now()); err != nil {
			t.Fatalf("opener %d write: %v", i, err)
		}
	}
}

func TestPruneBoundary(t *testing.T) {
	s, _ := newStore(t)
	young := insertDelivered(t, s, 29*24*time.Hour)
	old := insertDelivered(t, s, 31*24*time.Hour)

	res, err := s.PruneNow(t.Context(), DefaultRetention, false)
	if err != nil {
		t.Fatalf("PruneNow: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", res.Deleted)
	}
	if !messageExists(t, s, young) {
		t.Error("29-day-old message was deleted, want kept")
	}
	if messageExists(t, s, old) {
		t.Error("31-day-old message survived, want deleted")
	}
}

func TestPruneNowDryRunDeletesNothing(t *testing.T) {
	s, _ := newStore(t)
	insertDelivered(t, s, 31*24*time.Hour)

	res, err := s.PruneNow(t.Context(), DefaultRetention, true)
	if err != nil {
		t.Fatalf("PruneNow dry-run: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("dry-run reported %d prunable, want 1", res.Deleted)
	}
	if n := countMessages(t, s); n != 1 {
		t.Errorf("dry-run deleted rows: %d remain, want 1", n)
	}
}

// TestPruneSkipsUndelivered guards the injection-window rule: a message that
// has aged past retention but was never delivered is not garbage, it is
// undelivered mail.
func TestPruneSkipsUndelivered(t *testing.T) {
	s, _ := newStore(t)
	created := s.Now() - (31 * 24 * time.Hour).Milliseconds()
	if _, err := s.db.ExecContext(t.Context(),
		"INSERT INTO messages(alias, body, hop, origin, created_at) VALUES ('inbox', 'b', 0, 'human', ?)",
		created); err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := s.PruneNow(t.Context(), DefaultRetention, false)
	if err != nil {
		t.Fatalf("PruneNow: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("deleted = %d, want 0", res.Deleted)
	}
}

// TestPruneSkipsReferenced covers the foreign-key hazard: rows still named by
// sessions.last_inbound_id or messages.cause_id must be left alone. Deleting
// them would fail the statement outright with foreign_keys=ON, leaving prune
// permanently unable to make progress.
func TestPruneSkipsReferenced(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	old := (31 * 24 * time.Hour).Milliseconds()

	causeID := insertDelivered(t, s, 31*24*time.Hour)
	inboundID := insertDelivered(t, s, 31*24*time.Hour)
	plainID := insertDelivered(t, s, 31*24*time.Hour)

	// A live session whose causal representative is one of the expired rows.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions(session_id, tool, root, heartbeat_at, last_inbound_id, last_inbound_seq)
		VALUES ('s1', 'claude', '/tmp/w', ?, ?, 1)`, s.Now(), inboundID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	// A later message caused by another expired row. It is itself expired and
	// unreferenced, so it is a legitimate candidate — deleting the child while
	// keeping its cause is exactly the asymmetry the exclusion rules create.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO messages(alias, body, hop, origin, cause_id, created_at, delivered_at)
		VALUES ('inbox', 'child', 1, 'caused', ?, ?, ?)`,
		causeID, s.Now()-old, s.Now()-old)
	if err != nil {
		t.Fatalf("insert caused message: %v", err)
	}
	childID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("child id: %v", err)
	}

	pruned, err := s.PruneNow(ctx, DefaultRetention, false)
	if err != nil {
		t.Fatalf("PruneNow: %v", err) // an FK violation would surface here
	}

	if !messageExists(t, s, causeID) {
		t.Error("cause_id-referenced message was deleted")
	}
	if !messageExists(t, s, inboundID) {
		t.Error("last_inbound_id-referenced message was deleted")
	}
	if messageExists(t, s, plainID) {
		t.Error("unreferenced expired message survived")
	}
	if messageExists(t, s, childID) {
		t.Error("expired unreferenced child survived")
	}
	if pruned.Deleted != 2 {
		t.Errorf("deleted = %d, want 2 (the unreferenced row and the child)", pruned.Deleted)
	}
}

// TestPruneGateOnce forces concurrent gate acquisition. Exactly one caller may
// win, and while that one is still working nobody else may start.
func TestPruneGateOnce(t *testing.T) {
	s, fake := newStore(t)
	const runners = 8

	start := make(chan struct{})
	var wg sync.WaitGroup
	tokens := make([]string, runners)
	errs := make([]error, runners)

	for i := range runners {
		wg.Go(func() {
			<-start
			tokens[i], errs[i] = s.acquirePruneGate(context.Background())
		})
	}
	close(start)
	wg.Wait()

	winners := 0
	var held string
	for i, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: %v", i, err)
		}
		if tokens[i] != "" {
			winners++
			held = tokens[i]
		}
	}
	if winners != 1 {
		t.Fatalf("%d runners won the gate, want 1", winners)
	}

	// Still in progress. Advancing by less than the lease keeps the holder's
	// claim valid, which isolates the started_at guard: the done_at clause is
	// already satisfied (it is still NULL), so only the in-progress condition
	// can be what keeps the newcomer out.
	fake.Advance(pruneLease / 2)
	token, err := s.acquirePruneGate(t.Context())
	if err != nil {
		t.Fatalf("acquire during in-progress prune: %v", err)
	}
	if token != "" {
		t.Error("acquired the gate while a prune was in progress")
	}

	// Once the holder finishes and the interval passes, the gate reopens.
	if ok, err := s.releasePruneGate(t.Context(), held); err != nil || !ok {
		t.Fatalf("release: ok=%v err=%v", ok, err)
	}
	if token, err = s.acquirePruneGate(t.Context()); err != nil {
		t.Fatalf("acquire right after release: %v", err)
	}
	if token != "" {
		t.Error("acquired the gate before the interval elapsed")
	}
	fake.Advance(pruneGate + time.Minute)
	if token, err = s.acquirePruneGate(t.Context()); err != nil {
		t.Fatalf("acquire after interval: %v", err)
	}
	if token == "" {
		t.Error("gate stayed shut after the interval elapsed")
	}
}

// TestPruneDoneRequiresToken covers the stranded-runner handover: once a
// successor has taken the gate, the original runner finishing late must not
// stamp its completion over the successor's in-progress state.
func TestPruneDoneRequiresToken(t *testing.T) {
	s, fake := newStore(t)

	first, err := s.acquirePruneGate(t.Context())
	if err != nil || first == "" {
		t.Fatalf("first acquire: token=%q err=%v", first, err)
	}

	// The first runner stalls past its lease and is presumed stranded.
	fake.Advance(pruneGate + pruneLease + time.Minute)
	second, err := s.acquirePruneGate(t.Context())
	if err != nil || second == "" {
		t.Fatalf("takeover acquire: token=%q err=%v", second, err)
	}

	// The first runner finally finishes. Its completion must be rejected.
	ok, err := s.releasePruneGate(t.Context(), first)
	if err != nil {
		t.Fatalf("stale release: %v", err)
	}
	if ok {
		t.Error("stale runner recorded completion, want rejected")
	}

	var doneAt any
	var heldToken string
	if err := s.db.QueryRowContext(t.Context(),
		"SELECT prune_done_at, lease_token FROM meta WHERE key = 'prune'").Scan(&doneAt, &heldToken); err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if doneAt != nil {
		t.Errorf("prune_done_at = %v, want NULL (successor still running)", doneAt)
	}
	if heldToken != second {
		t.Errorf("lease_token = %q, want the successor's %q", heldToken, second)
	}

	// The successor's own completion is accepted.
	if ok, err := s.releasePruneGate(t.Context(), second); err != nil || !ok {
		t.Fatalf("successor release: ok=%v err=%v", ok, err)
	}
}

// TestPruneOpportunisticRunsOnce is the end-to-end gate behaviour a hook sees:
// the first call prunes, an immediate second call does nothing.
func TestPruneOpportunisticRunsOnce(t *testing.T) {
	s, fake := newStore(t)
	insertDelivered(t, s, 31*24*time.Hour)

	first, err := s.PruneOpportunistic(t.Context(), DefaultRetention)
	if err != nil {
		t.Fatalf("first prune: %v", err)
	}
	if !first.Ran || first.Deleted != 1 {
		t.Fatalf("first prune = %+v, want {Ran:true Deleted:1}", first)
	}

	insertDelivered(t, s, 31*24*time.Hour)
	second, err := s.PruneOpportunistic(t.Context(), DefaultRetention)
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if second.Ran {
		t.Error("second prune ran inside the gate interval")
	}
	if n := countMessages(t, s); n != 1 {
		t.Errorf("%d messages remain, want 1 (second prune must not delete)", n)
	}

	fake.Advance(pruneGate + time.Minute)
	third, err := s.PruneOpportunistic(t.Context(), DefaultRetention)
	if err != nil {
		t.Fatalf("third prune: %v", err)
	}
	if !third.Ran || third.Deleted != 1 {
		t.Errorf("third prune = %+v, want {Ran:true Deleted:1}", third)
	}
}

// TestHostKeyUniqueness pins the session-binding invariant that priority-3
// resolution rests on: at most one session row may hold a given host key.
func TestHostKeyUniqueness(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()
	insert := func(id string, pid any) error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO sessions(session_id, tool, root, heartbeat_at, host_client, host_pid, host_start)
			VALUES (?, 'claude', '/tmp/w', ?, 'claude', ?, 100)`, id, s.Now(), pid)
		return err
	}

	if err := insert("s1", 4242); err != nil {
		t.Fatalf("first session: %v", err)
	}
	if err := insert("s2", 4242); err == nil {
		t.Error("a second session took the same host key, want a uniqueness failure")
	}
	// Sessions with no host binding are unconstrained — many may coexist.
	if err := insert("s3", nil); err != nil {
		t.Fatalf("unbound session: %v", err)
	}
	if err := insert("s4", nil); err != nil {
		t.Fatalf("second unbound session: %v", err)
	}
}
