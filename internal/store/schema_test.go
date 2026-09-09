package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/iMUngHee/pager/internal/clock"
)

// These cover the upgrade path: migrate replaying an ordered log rather than
// branching on the version it happens to find. The database this project ships
// against is one shared file that several processes open on every prompt, so a
// broken upgrade does not fail a request — it stops the tool opening at all.

// openAtVersion creates a database at the given schema version and returns its
// path, without going through Open (which would migrate it to current).
//
// The file is brought up through dsn and applyWAL first, on purpose. applyWAL
// runs before migrate inside Open, and the journal_mode switch is the one
// operation SQLite runs no busy handler for — a fixture that skipped it would
// leave concurrent openers racing that switch instead of the migration, and the
// failure would read as a migration bug.
func openAtVersion(t *testing.T, version int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".pager", "msg.db")
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		t.Fatalf("create dir: %v", err)
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close() //nolint:errcheck // fixture handle
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := applyWAL(t.Context(), db); err != nil {
		t.Fatalf("applyWAL: %v", err)
	}
	for v := 0; v < version; v++ {
		if _, err := db.ExecContext(t.Context(), migrations[v]); err != nil {
			t.Fatalf("seed migration %d: %v", v, err)
		}
	}
	if _, err := db.ExecContext(t.Context(),
		fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("seed user_version: %v", err)
	}
	return path
}

// userVersion reads the schema version a database reports.
func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close() //nolint:errcheck // probe handle
	var v int
	if err := db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

// setUserVersion forces a version onto a database, standing in for an outside
// writer or a corrupt header.
func setUserVersion(t *testing.T, path string, v int) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close() //nolint:errcheck // probe handle
	if _, err := db.ExecContext(t.Context(),
		fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		t.Fatalf("set user_version = %d: %v", v, err)
	}
}

// schemaOf renders everything a database's structure consists of.
//
// sqlite_master rather than table_info(messages): the narrower reading would
// miss notnull, dflt_value and pk, every index including the partial ones, and
// the sessions/aliases/meta tables entirely. The seeded meta row goes with it
// because ddl plants it, so a migration that changed it would otherwise slip
// past. Comparable byte for byte, because ADD COLUMN splices a column into the
// stored SQL text deterministically and both paths run the same statements.
func schemaOf(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close() //nolint:errcheck // probe handle

	var out string
	rows, err := db.QueryContext(t.Context(),
		"SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name")
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var kind, name, tbl, ddlText string
		if err := rows.Scan(&kind, &name, &tbl, &ddlText); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		out += fmt.Sprintf("%s %s %s\n%s\n", kind, name, tbl, ddlText)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}

	keys, err := db.QueryContext(t.Context(), "SELECT key FROM meta ORDER BY key")
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	defer keys.Close() //nolint:errcheck
	for keys.Next() {
		var k string
		if err := keys.Scan(&k); err != nil {
			t.Fatalf("scan meta: %v", err)
		}
		out += "meta " + k + "\n"
	}
	if err := keys.Err(); err != nil {
		t.Fatalf("read meta: %v", err)
	}
	return out
}

// hasColumn reports whether a table carries a column.
func hasColumn(t *testing.T, path, table, column string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close() //nolint:errcheck // probe handle
	var n int
	if err := db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM pragma_table_info(?) WHERE name = ?", table, column).Scan(&n); err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	return n > 0
}

// TestMigrationsAreAppendOnly is what actually enforces the frozen-log rule.
//
// The schema comparison below cannot: its fixture builds a v1 database from
// migrations[0], which IS ddl, so editing ddl changes both sides of that
// comparison identically and it passes green. Meanwhile a database created by
// the old ddl comes up at version 1, replays only the entries after it, and
// never receives the new column — and the resulting "no such column" is
// swallowed in a hook. Digests catch that at the point of violation.
//
// A new migration appends an entry and its digest here. Editing an existing
// entry is the mistake, and this is the message that says so.
func TestMigrationsAreAppendOnly(t *testing.T) {
	want := []string{
		"9b773708f612d2ef8b37f61bc88dd2929c516f2616ef739279f1e270b917dec2", // 0 -> 1 (ddl)
		"8b11f1573b1ecc6b99fd42ea5ae1dd367c4bcc7e49bf546615c5ae4e471ce802", // 1 -> 2 (listed_at)
		"fa2044e78bcdadaa98616e6bea55f78f2e27910d4497ff96dbdf2b2dcc345747", // 2 -> 3 (purpose)
	}
	if len(want) != len(migrations) {
		t.Fatalf("%d migrations but %d digests — append the new entry's digest", len(migrations), len(want))
	}
	for i, m := range migrations {
		sum := sha256.Sum256([]byte(m))
		got := hex.EncodeToString(sum[:])
		if got != want[i] {
			t.Errorf("migrations[%d] digest = %s, want %s\n"+
				"you edited a frozen migration; append a new entry instead", i, got, want[i])
		}
	}
}

// TestUpgradesAVersionOneDatabase is the path that has never run in production.
func TestUpgradesAVersionOneDatabase(t *testing.T) {
	path := openAtVersion(t, 1)
	if got := userVersion(t, path); got != 1 {
		t.Fatalf("fixture is at version %d, want 1", got)
	}
	if hasColumn(t, path, "messages", "listed_at") {
		t.Fatal("the v1 fixture already has listed_at — the fixture is not v1")
	}

	s, err := Open(t.Context(), path, clock.NewFake(testBase))
	if err != nil {
		t.Fatalf("Open a v1 database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if got := userVersion(t, path); got != schemaVersion {
		t.Errorf("user_version = %d after opening, want %d", got, schemaVersion)
	}
	if !hasColumn(t, path, "messages", "listed_at") {
		t.Error("listed_at is missing after the upgrade")
	}
}

// TestUpgradeAddsThePurposeColumn is the same shape as the listed_at upgrade
// above, for the column msg_roster reads.
//
// The fixture is schemaVersion-1 rather than schemaVersion, and the difference
// is the whole test: openAtVersion replays every entry below its argument, so
// seeding at the current version would hand back a database that already has
// the column and the assertion below would pass without Open doing anything.
func TestUpgradeAddsThePurposeColumn(t *testing.T) {
	path := openAtVersion(t, schemaVersion-1)
	if hasColumn(t, path, "sessions", "purpose") {
		t.Fatal("the fixture already has purpose — it was not seeded below the migration that adds it")
	}

	s, err := Open(t.Context(), path, clock.NewFake(testBase))
	if err != nil {
		t.Fatalf("Open a database one version behind: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if got := userVersion(t, path); got != schemaVersion {
		t.Errorf("user_version = %d after opening, want %d", got, schemaVersion)
	}
	if !hasColumn(t, path, "sessions", "purpose") {
		t.Error("purpose is missing after the upgrade")
	}
}

// TestFreshAndUpgradedSchemasMatch pins the property the replay log exists for:
// a database that ran every entry from empty and one that joined partway must
// end up structurally identical.
func TestFreshAndUpgradedSchemasMatch(t *testing.T) {
	fresh := openAtVersion(t, 0)
	fs, err := Open(t.Context(), fresh, clock.NewFake(testBase))
	if err != nil {
		t.Fatalf("open fresh: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	upgraded := openAtVersion(t, 1)
	us, err := Open(t.Context(), upgraded, clock.NewFake(testBase))
	if err != nil {
		t.Fatalf("open upgraded: %v", err)
	}
	t.Cleanup(func() { _ = us.Close() })

	if a, b := schemaOf(t, fresh), schemaOf(t, upgraded); a != b {
		t.Errorf("a fresh database and an upgraded one differ.\nfresh:\n%s\nupgraded:\n%s", a, b)
	}
}

// TestUpgradePreservesRows checks the obvious thing the ALTER must not do.
func TestUpgradePreservesRows(t *testing.T) {
	path := openAtVersion(t, 1)

	seed, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open for seeding: %v", err)
	}
	if _, err := seed.ExecContext(t.Context(), `
		INSERT INTO messages(alias, body, hop, origin, created_at)
		VALUES ('inbox', 'survives the upgrade', 0, 'human', 1)`); err != nil {
		t.Fatalf("seed a message: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seeding handle: %v", err)
	}

	s, err := Open(t.Context(), path, clock.NewFake(testBase))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var body string
	var listed any
	if err := s.db.QueryRowContext(t.Context(),
		"SELECT body, listed_at FROM messages WHERE alias = 'inbox'").Scan(&body, &listed); err != nil {
		t.Fatalf("read the seeded message: %v", err)
	}
	if body != "survives the upgrade" {
		t.Errorf("body = %q after the upgrade", body)
	}
	if listed != nil {
		t.Errorf("listed_at = %v on an existing row, want NULL", listed)
	}
}

// TestRejectsAnOutOfRangeVersion covers both ends. Above the binary's version a
// downgrade cannot know what changed. Below zero there is no such version at
// all — and user_version is a signed field an outside writer can set, so without
// the lower bound the replay loop would index the log negatively and panic,
// which in a hook exits non-zero with a stack trace on a session's stderr.
func TestRejectsAnOutOfRangeVersion(t *testing.T) {
	for _, version := range []int{schemaVersion + 1, -1, math.MinInt32} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := openAtVersion(t, 1)
			setUserVersion(t, path, version)

			s, err := Open(t.Context(), path, clock.NewFake(testBase))
			if err == nil {
				_ = s.Close()
				t.Fatalf("Open accepted version %d", version)
			}
		})
	}
}

// TestAFailedMigrationLeavesTheVersionAlone pins the reason the upgrade runs
// inside the caller's transaction. Recording a version whose statements did not
// all succeed is the one outcome that cannot be recovered from automatically:
// the next open would skip the entries it thinks already ran.
func TestAFailedMigrationLeavesTheVersionAlone(t *testing.T) {
	path := openAtVersion(t, 1)

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	if _, err := conn.ExecContext(t.Context(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	broken := []string{
		migrations[0],
		`ALTER TABLE messages ADD COLUMN listed_at INTEGER`,
		`this is not sql`,
	}
	if err := applyMigrations(t.Context(), conn, 1, broken); err == nil {
		t.Fatal("applyMigrations reported success on a broken entry")
	}
	if _, err := conn.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if got := userVersion(t, path); got != 1 {
		t.Errorf("user_version = %d after a failed migration, want the unchanged 1", got)
	}
	if hasColumn(t, path, "messages", "listed_at") {
		t.Error("listed_at survived a rolled-back migration")
	}
}

// TestAnAlreadyCurrentOpenWritesNothing guards the hottest path in the project.
//
// Once a database is current, every hook on every prompt still calls Open, and
// the replay must come out a true no-op rather than rewriting the same version
// under BEGIN EXCLUSIVE. The switch this replaced got that for free with an
// empty case; the loop gets it from the early return, and deleting that return
// leaves every test passing because the loop runs zero times and the rewrite is
// idempotent. data_version is what makes the difference observable: it advances
// on another connection's write even when the value written is unchanged, and
// stays put for a read-only transaction.
func TestAnAlreadyCurrentOpenWritesNothing(t *testing.T) {
	path := openAtVersion(t, 0)
	first, err := Open(t.Context(), path, clock.NewFake(testBase))
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	watcher, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open watcher: %v", err)
	}
	defer watcher.Close() //nolint:errcheck // probe handle
	dataVersion := func() int {
		t.Helper()
		var v int
		if err := watcher.QueryRowContext(t.Context(), "PRAGMA data_version").Scan(&v); err != nil {
			t.Fatalf("read data_version: %v", err)
		}
		return v
	}

	before := dataVersion()
	second, err := Open(t.Context(), path, clock.NewFake(testBase))
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if got := dataVersion(); got != before {
		t.Errorf("opening an already-current database wrote to it (data_version %d -> %d)", before, got)
	}
}

// TestTheRecordedVersionIsWhatWasReplayed pins the version applyMigrations
// stamps to the log it was handed rather than to the package constant.
//
// The two are equal in production because migrate always passes migrations, so
// substituting schemaVersion would pass every other test. A list of a different
// length is the only thing that tells them apart.
func TestTheRecordedVersionIsWhatWasReplayed(t *testing.T) {
	path := openAtVersion(t, 1)

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	// Four entries, joined at 1: three run, and the version recorded must be 4 —
	// the length of what ran, not schemaVersion.
	//
	// The list grows whenever a real migration lands, because its whole job is
	// to be a different length from schemaVersion. The guard below is what says
	// so: it fired when the purpose migration made the real log three long,
	// which is the failure that stops this test from quietly becoming a
	// tautology.
	longer := []string{
		migrations[0],
		`ALTER TABLE messages ADD COLUMN listed_at INTEGER`,
		`ALTER TABLE messages ADD COLUMN probe_only INTEGER`,
		`ALTER TABLE messages ADD COLUMN probe_only_too INTEGER`,
	}
	if len(longer) == schemaVersion {
		t.Fatalf("the fixture list is %d long, the same as schemaVersion — it cannot tell them apart", len(longer))
	}

	if _, err := conn.ExecContext(t.Context(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := applyMigrations(t.Context(), conn, 1, longer); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	if _, err := conn.ExecContext(t.Context(), "COMMIT"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if got := userVersion(t, path); got != len(longer) {
		t.Errorf("user_version = %d after replaying %d entries, want %d", got, len(longer), len(longer))
	}
}

// TestConcurrentUpgrade is the shape a real upgrade takes: several processes
// open the shared database at once, all of them wanting to migrate it.
//
// "Every opener succeeds" is the assertion rather than a weak smoke check —
// running the ALTER twice fails with duplicate column name, so a lost race
// would surface here as an error rather than as silence.
func TestConcurrentUpgrade(t *testing.T) {
	path := openAtVersion(t, 1)
	const openers = 8

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, openers)
	stores := make([]*Store, openers)

	for i := range openers {
		wg.Go(func() {
			<-start // barrier: force the upgrades to actually overlap
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
	if got := userVersion(t, path); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}

	// The column must exist once. A second ALTER would have errored above, but
	// asserting the count states what "once" means.
	var n int
	if err := stores[0].db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM pragma_table_info('messages') WHERE name = 'listed_at'").Scan(&n); err != nil {
		t.Fatalf("count listed_at: %v", err)
	}
	if n != 1 {
		t.Errorf("listed_at appears %d times, want 1", n)
	}

	// And every opener came away with a usable handle, not one holding a
	// transaction its migration left open.
	for i, s := range stores {
		if _, err := s.Exec(t.Context(), `
			INSERT INTO messages(alias, body, hop, origin, created_at)
			VALUES ('inbox', 'after the upgrade', 0, 'human', ?)`, i); err != nil {
			t.Errorf("opener %d cannot write after upgrading: %v", i, err)
		}
	}
}
