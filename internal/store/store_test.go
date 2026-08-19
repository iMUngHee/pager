package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// writeLockHeld reports whether some connection outside this call still owns
// the database's write lock, asked through a pool the test does not otherwise
// touch.
//
// The probe lowers its own busy_timeout: five seconds is the right budget for a
// real writer, but here a held lock is a possible answer rather than a fault,
// and waiting the full DSN timeout for it would cost every call five seconds.
func writeLockHeld(t *testing.T, path string) bool {
	t.Helper()
	probe, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	defer probe.Close() //nolint:errcheck // probe pool, nothing to flush
	conn, err := probe.Conn(t.Context())
	if err != nil {
		t.Fatalf("probe connection: %v", err)
	}
	defer conn.Close() //nolint:errcheck // returning the conn to the probe pool
	if _, err := conn.ExecContext(t.Context(), "PRAGMA busy_timeout = 200"); err != nil {
		t.Fatalf("probe busy_timeout: %v", err)
	}
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		if isBusy(err) {
			return true
		}
		t.Fatalf("probe begin: %v", err)
	}
	if _, err := conn.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatalf("probe rollback: %v", err)
	}
	return false
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

// insertLeaky writes one row through c. The tests below need a transaction that
// has actually changed something, so that an unrolled-back one is visible as
// data rather than only as a held lock.
func insertLeaky(ctx context.Context, c *sql.Conn, s *Store, body string) error {
	_, err := c.ExecContext(ctx, `
		INSERT INTO messages(alias, body, hop, origin, created_at)
		VALUES ('inbox', ?, 0, 'human', ?)`, body, s.Now())
	return err
}

// TestWriteTxRollsBackWhenContextIsCancelled pins the failure an expired hook
// deadline used to cause.
//
// The driver returns before sending any SQL once the context is done, so a
// ROLLBACK issued on that context never reaches SQLite. database/sql then pools
// a connection still inside a transaction, and neither it nor the driver
// inspects for one — so the next writer's BEGIN fails, and it keeps failing,
// while a single-statement Exec silently joins the stale transaction instead of
// committing.
func TestWriteTxRollsBackWhenContextIsCancelled(t *testing.T) {
	s, _ := newStore(t)
	// One connection, so the connection the cancelled transaction leaves behind
	// is provably the one the next caller is handed. An unbounded pool could
	// give the second WriteTx a fresh connection and pass without proving
	// anything.
	s.db.SetMaxOpenConns(1)

	ctx, cancel := context.WithCancel(t.Context())
	err := s.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		if err := insertLeaky(ctx, c, s, "leaked"); err != nil {
			return err
		}
		cancel() // the hook deadline expires between statements
		return errors.New("write failed after the deadline expired")
	})
	if err == nil {
		t.Fatal("WriteTx returned nil, want the failure its body reported")
	}

	// The transaction has to be gone from the connection, not merely abandoned
	// on it: this is the assertion that fails before the fix.
	if err := s.WriteTx(t.Context(), func(ctx context.Context, c *sql.Conn) error {
		return insertLeaky(ctx, c, s, "later")
	}); err != nil {
		t.Fatalf("WriteTx after a cancelled one: %v", err)
	}
	if writeLockHeld(t, s.path) {
		t.Error("the write lock is still held after the cancelled transaction")
	}

	var leaked int
	if err := s.db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM messages WHERE body = 'leaked'").Scan(&leaked); err != nil {
		t.Fatalf("count leaked rows: %v", err)
	}
	if leaked != 0 {
		t.Errorf("the cancelled transaction left %d row(s) behind, want 0", leaked)
	}
}

// TestWriteTxDiscardsConnectionWhenRollbackFails covers the other half of the
// contract. A rollback that fails leaves the connection inside a transaction,
// and returning it to the pool would poison every later caller, so it has to be
// dropped instead.
//
// Committing inside the body and then reporting failure is the deterministic
// way to reach a rollback with no transaction left to undo — the same state an
// interrupted COMMIT reaches by chance.
func TestWriteTxDiscardsConnectionWhenRollbackFails(t *testing.T) {
	s, _ := newStore(t)
	s.db.SetMaxOpenConns(1)

	err := s.WriteTx(t.Context(), func(ctx context.Context, c *sql.Conn) error {
		if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		return errors.New("failed after the transaction had already committed")
	})
	if err == nil {
		t.Fatal("WriteTx returned nil, want the failure its body reported")
	}

	if idle := s.db.Stats().Idle; idle != 0 {
		t.Errorf("the connection went back to the pool (Idle=%d), want it discarded", idle)
	}
	if err := s.WriteTx(t.Context(), func(ctx context.Context, c *sql.Conn) error {
		return insertLeaky(ctx, c, s, "later")
	}); err != nil {
		t.Fatalf("WriteTx after a discarded connection: %v", err)
	}
}

// TestRollbackTxReleasesTheWriteLock exercises the helper on migrate's lock
// mode. migrate has no seam for injecting a cancellation — its statements are
// fixed and it runs inside Open — so this drives BEGIN EXCLUSIVE directly and
// asserts the property migrate depends on: after a cancelled transaction the
// lock is gone and the connection carries nothing forward.
func TestRollbackTxReleasesTheWriteLock(t *testing.T) {
	s, _ := newStore(t)
	s.db.SetMaxOpenConns(1)

	ctx, cancel := context.WithCancel(t.Context())
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatalf("connection: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("begin exclusive: %v", err)
	}
	if err := insertLeaky(ctx, conn, s, "leaked"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	cancel()

	rollbackTx(ctx, conn)
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("close connection: %v", err)
	}

	if writeLockHeld(t, s.path) {
		t.Error("the write lock is still held after rollbackTx")
	}
	var leaked int
	if err := s.db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM messages WHERE body = 'leaked'").Scan(&leaked); err != nil {
		t.Fatalf("count leaked rows: %v", err)
	}
	if leaked != 0 {
		t.Errorf("rollbackTx left %d row(s) behind, want 0", leaked)
	}
}

// TestManualPruneYieldsToARunningPrune is the exclusion half of the contract.
//
// A manual prune has to bypass the once-a-day interval — that is what asking for
// it means — but not the exclusion. The archive append writes at an absolute
// offset outside any transaction, so a second prune starting mid-append computes
// the same offset and overwrites the first one's records, leaving a file that
// still parses and is simply missing them.
func TestManualPruneYieldsToARunningPrune(t *testing.T) {
	s, _ := newStore(t)
	insertDelivered(t, s, DefaultRetention+time.Hour)
	before := countMessages(t, s)

	// Somebody else is mid-prune: the lease is held and not yet released.
	held, err := s.acquirePruneGate(t.Context())
	if err != nil || held == "" {
		t.Fatalf("seed the in-progress lease: token=%q err=%v", held, err)
	}

	res, err := s.PruneNow(t.Context(), DefaultRetention, false)
	if err != nil {
		t.Fatalf("PruneNow: %v", err)
	}
	if res.Ran {
		t.Error("PruneNow ran while another prune held the lease")
	}
	if res.Deleted != 0 {
		t.Errorf("deleted = %d while another prune held the lease, want 0", res.Deleted)
	}
	if got := countMessages(t, s); got != before {
		t.Errorf("message count is %d, want %d — the yielding prune still deleted", got, before)
	}
}

// TestConcurrentPrunesLoseNoArchiveRecords states the invariant the archive
// exists for: nothing leaves the database without being in the file first.
//
// Both entry points run against one database at once, which is the shape a
// manual `pager prune` and a hook's opportunistic prune make.
func TestConcurrentPrunesLoseNoArchiveRecords(t *testing.T) {
	s, _ := newStore(t)
	const seeded = 12
	ids := make([]int64, seeded)
	for i := range ids {
		ids[i] = insertDelivered(t, s, DefaultRetention+time.Hour)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		if _, err := s.PruneNow(context.Background(), DefaultRetention, false); err != nil {
			t.Errorf("PruneNow: %v", err)
		}
	})
	wg.Go(func() {
		<-start
		if _, err := s.PruneOpportunistic(context.Background(), DefaultRetention); err != nil {
			t.Errorf("PruneOpportunistic: %v", err)
		}
	})
	close(start)
	wg.Wait()

	archived := archivedIDs(t, s)
	for _, id := range ids {
		if !messageExists(t, s, id) && !slices.Contains(archived, id) {
			t.Errorf("message %d left the database without reaching the archive", id)
		}
	}
}

// TestManualPruneIgnoresTheInterval is the other half: taking the lease must not
// have made `pager prune` wait for the daily slot. Asking for a prune is the
// whole reason the command exists.
func TestManualPruneIgnoresTheInterval(t *testing.T) {
	s, _ := newStore(t)
	id := insertDelivered(t, s, DefaultRetention+time.Hour)

	// A scheduled prune has just finished, so the interval is closed.
	token, err := s.acquirePruneGate(t.Context())
	if err != nil || token == "" {
		t.Fatalf("seed a completed prune: token=%q err=%v", token, err)
	}
	if ok, err := s.releasePruneGate(t.Context(), token); err != nil || !ok {
		t.Fatalf("release: ok=%v err=%v", ok, err)
	}
	if shut, err := s.acquirePruneGate(t.Context()); err != nil || shut != "" {
		t.Fatalf("the interval is not closed: token=%q err=%v", shut, err)
	}

	res, err := s.PruneNow(t.Context(), DefaultRetention, false)
	if err != nil {
		t.Fatalf("PruneNow: %v", err)
	}
	if !res.Ran || res.Deleted != 1 {
		t.Errorf("PruneNow = %+v, want Ran with 1 deleted — it waited for the interval", res)
	}
	if messageExists(t, s, id) {
		t.Error("the expired message survived a manual prune")
	}
}

// TestManualPruneLeavesTheIntervalUnadvanced pins the release semantics. A
// manual prune drops its lease without recording completion, so it does not
// consume the day's scheduled slot — that would be a retention-policy change
// rather than the serialisation the lease is for.
func TestManualPruneLeavesTheIntervalUnadvanced(t *testing.T) {
	s, _ := newStore(t)
	insertDelivered(t, s, DefaultRetention+time.Hour)

	if _, err := s.PruneNow(t.Context(), DefaultRetention, false); err != nil {
		t.Fatalf("PruneNow: %v", err)
	}

	var doneAt any
	var heldToken any
	if err := s.db.QueryRowContext(t.Context(),
		"SELECT prune_done_at, lease_token FROM meta WHERE key = 'prune'").Scan(&doneAt, &heldToken); err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if doneAt != nil {
		t.Errorf("prune_done_at = %v after a manual prune, want NULL", doneAt)
	}
	if heldToken != nil {
		t.Errorf("lease_token = %v after a manual prune, want NULL", heldToken)
	}

	// The scheduled prune therefore still gets its turn.
	res, err := s.PruneOpportunistic(t.Context(), DefaultRetention)
	if err != nil {
		t.Fatalf("PruneOpportunistic: %v", err)
	}
	if !res.Ran {
		t.Error("the manual prune consumed the scheduled slot")
	}
}

// TestManualPruneReleasesTheLeaseOnFailure keeps the documented recovery path
// working. README points at `pager prune` erroring as how an archive problem
// surfaces, so the retry after fixing the cause must not be locked out for the
// lease interval.
func TestManualPruneReleasesTheLeaseOnFailure(t *testing.T) {
	t.Setenv("PAGER_ARCHIVE", "")
	s, _ := newStore(t)
	insertDelivered(t, s, DefaultRetention+time.Hour)

	// A directory standing where the archive file belongs makes the open fail.
	if err := os.Mkdir(archivePath(s.path), 0o700); err != nil {
		t.Fatalf("block archive path: %v", err)
	}

	if _, err := s.PruneNow(t.Context(), DefaultRetention, false); err == nil {
		t.Fatal("PruneNow succeeded although the archive could not be written")
	}

	// The retry must reach the same failure rather than be turned away. Ran
	// distinguishes the two: a retry locked out by the lease reports Ran false
	// with no error, which reads as "nothing to do" and hides the real problem.
	res, err := s.PruneNow(t.Context(), DefaultRetention, false)
	if !res.Ran {
		t.Fatal("the retry was locked out by the failed prune's lease")
	}
	if err == nil {
		t.Error("the retry reported success although the archive is still blocked")
	}
}
