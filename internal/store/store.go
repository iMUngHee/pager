// Package store owns the pager database: schema, connection policy, and the
// retention prune.
//
// One file (~/.pager/msg.db) holds every workspace's sessions, aliases, and
// messages. Lock consistency is therefore a property of that file rather than
// something the callers have to coordinate.
//
// Every timestamp column in the schema is Unix milliseconds, taken from the
// injected clock.Clock so tests can move time without sleeping.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/unghee/pager/internal/clock"
)

// The database is local-user-only and carries message bodies written by other
// sessions, so neither the directory nor the file is group- or world-readable.
const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// busyRetries bounds the retry loop for a writer that still sees SQLITE_BUSY
// after the driver's own busy_timeout has already elapsed.
const busyRetries = 3

// Store is an open pager database.
type Store struct {
	db    *sql.DB
	clock clock.Clock
	// path is the database this store actually opened. The archive lives beside
	// it, so isolating a test or a second set of sessions with PAGER_DB has to
	// isolate the archive too — reading the environment again at archive time
	// would not see the path this store was handed.
	path string
}

// DefaultPath returns the database location: PAGER_DB when set, otherwise
// ~/.pager/msg.db.
//
// The override exists so a test run — or a second, isolated set of sessions —
// never has to touch the real inbox.
func DefaultPath() (string, error) {
	if override := os.Getenv("PAGER_DB"); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".pager", "msg.db"), nil
}

// Open opens the database at path — creating the directory and file when
// missing — and migrates it to the current schema.
func Open(ctx context.Context, path string, c clock.Clock) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	// MkdirAll is subject to umask and leaves an existing directory alone, so
	// state the permission explicitly in both cases.
	if err := os.Chmod(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("chmod %s: %w", dir, err)
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// sql.Open is lazy; Ping is what actually creates the file, and the file
	// has to exist before its mode can be set.
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := os.Chmod(path, filePerm); err != nil {
		db.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := applyWAL(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, clock: c, path: path}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Now is the current time in Unix milliseconds, the unit of every timestamp
// column in the schema. It comes from the injected clock, so a test can move it.
func (s *Store) Now() int64 { return s.clock.Now().UnixMilli() }

// DB exposes the database. The deliver package composes its own statements —
// its correctness lives in exact SQL conditions rather than in Go control flow,
// so those statements belong beside the rules they enforce.
func (s *Store) DB() *sql.DB { return s.db }

// dsn builds the connection string.
//
// These three PRAGMAs are per-connection settings, and database/sql pools
// connections and opens new ones at will, so they must be in the DSN: issued
// once after Open they would configure exactly one pooled connection.
//
// journal_mode is deliberately absent — see applyWAL.
//
// Encoding is url.Values' job, since a database path may contain characters
// that would otherwise terminate the query string. TestConnectionPragmas
// asserts the settings actually take effect, so an encoding mismatch fails
// loudly rather than silently dropping foreign_keys.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	u := url.URL{Scheme: "file", Opaque: path, RawQuery: q.Encode()}
	return u.String()
}

// applyWAL switches the database to WAL journalling.
//
// journal_mode is not a per-connection setting like the DSN ones: it is a
// persistent property of the file, so it needs setting once and every later
// connection inherits it. Keeping it out of the DSN also confines the single
// operation that needs an exclusive lock to one place that can retry — SQLite
// does not run the busy handler for a journal_mode switch, so busy_timeout does
// not cover it and concurrent first-opens of a fresh database would otherwise
// fail Open outright. TestConcurrentMigrate is what catches a regression here.
func applyWAL(ctx context.Context, db *sql.DB) error {
	for attempt := 0; ; attempt++ {
		var mode string
		err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode)
		if err == nil {
			if mode != "wal" {
				return fmt.Errorf("journal_mode is %q after switching, want wal", mode)
			}
			return nil
		}
		if !isBusy(err) || attempt >= busyRetries {
			return fmt.Errorf("enable WAL: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * 10 * time.Millisecond):
		}
	}
}

// Exec runs a single write statement, retrying on SQLITE_BUSY.
//
// A single statement is its own transaction, which is all a conditional CAS
// needs; multi-statement work goes through WriteTx.
func (s *Store) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var err error
	for attempt := 0; ; attempt++ {
		var res sql.Result
		res, err = s.db.ExecContext(ctx, query, args...)
		if err == nil || !isBusy(err) || attempt >= busyRetries {
			return res, err
		}
		// busy_timeout already waited inside the driver, so this backoff is a
		// second line of defence rather than the primary one.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * 10 * time.Millisecond):
		}
	}
}

// WriteTx runs fn inside a BEGIN IMMEDIATE transaction on one dedicated
// connection, retrying the whole transaction on SQLITE_BUSY.
//
// IMMEDIATE takes the write lock up front. Every multi-statement write in pager
// reads state and then writes a decision derived from it — the host-key
// transfer here, and later the breaker count before an INSERT and the candidate
// re-validation before a claim. A deferred transaction would take the lock only
// at the first write, by which point the read it was based on may no longer
// hold.
//
// The transaction is driven with explicit statements for the same reason
// migrate is: database/sql cannot express IMMEDIATE.
func (s *Store) WriteTx(ctx context.Context, fn func(context.Context, *sql.Conn) error) error {
	for attempt := 0; ; attempt++ {
		err := s.writeTxOnce(ctx, fn)
		if err == nil || !isBusy(err) || attempt >= busyRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * 10 * time.Millisecond):
		}
	}
}

func (s *Store) writeTxOnce(ctx context.Context, fn func(context.Context, *sql.Conn) error) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("write connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck // returning the conn to the pool

	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin immediate: %w", err)
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	if err = fn(ctx, conn); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// isBusy reports whether err is SQLITE_BUSY. Extended result codes carry the
// primary code in the low byte, so mask before comparing.
func isBusy(err error) bool {
	var serr *sqlite.Error
	return errors.As(err, &serr) && serr.Code()&0xff == sqlite3.SQLITE_BUSY
}
