package store

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaVersion is the user_version this binary expects. Bump it and add a
// migration step whenever the DDL below changes.
const schemaVersion = 1

// ddl creates the whole schema. Table order matters: a foreign key must name a
// table that already exists, so messages (self-referential only) comes first,
// then sessions (→ messages), then aliases (→ sessions).
//
// The two foreign keys are not decoration — prune is required to skip rows that
// are still referenced, and foreign_keys=ON is what makes a mistake there fail
// loudly instead of silently corrupting a causal chain.
const ddl = `
CREATE TABLE messages (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  alias            TEXT    NOT NULL,           -- addressee at send time
  sender_session   TEXT,                       -- NULL for a --human send
  sender_label     TEXT    NOT NULL DEFAULT '',
  body             TEXT    NOT NULL,
  hop              INTEGER NOT NULL,           -- immutable causal depth, fixed at INSERT
  origin           TEXT    NOT NULL CHECK (origin IN ('human','caused')),
  cause_id         INTEGER REFERENCES messages(id),
  created_at       INTEGER NOT NULL,
  claim_token      TEXT,
  claim_epoch      INTEGER,                    -- recipient causal_epoch at claim time
  claimed_at       INTEGER,
  claim_expires_at INTEGER,
  delivery_seq     INTEGER,                    -- per-session arrival order, not depth
  delivered_at     INTEGER,
  CHECK ((origin = 'human') = (cause_id IS NULL))
);

CREATE INDEX messages_pending  ON messages(alias, delivered_at);
CREATE INDEX messages_lease    ON messages(claim_expires_at) WHERE delivered_at IS NULL;
CREATE INDEX messages_created  ON messages(created_at);
CREATE INDEX messages_cause    ON messages(cause_id) WHERE cause_id IS NOT NULL;

CREATE TABLE sessions (
  session_id       TEXT PRIMARY KEY,
  tool             TEXT    NOT NULL,           -- 'claude' | 'codex'
  root             TEXT    NOT NULL,           -- workspace; alias isolation axis with tool
  pm_ref           TEXT,                       -- optional KEY/id convenience
  heartbeat_at     INTEGER NOT NULL,           -- refreshed by every hook invocation
  causal_epoch     INTEGER NOT NULL DEFAULT 0, -- +1 on each verified user prompt
  seq_cursor       INTEGER NOT NULL DEFAULT 0, -- delivery_seq allocator
  last_inbound_id  INTEGER REFERENCES messages(id),
  last_inbound_seq INTEGER NOT NULL DEFAULT 0,
  claim_hint_at    INTEGER,                    -- orphan-alias hint, once per session
  host_client      TEXT,                       -- host identity: see docs/session-binding.md
  host_pid         INTEGER,
  host_start       INTEGER
);

-- The session-binding contract states that at most one row may hold a given
-- host key. Enforce it in the schema rather than trusting the hook's transfer
-- UPDATE: if that invariant ever broke, priority-3 resolution would silently
-- attribute a send to an arbitrary one of the matching sessions.
CREATE UNIQUE INDEX sessions_host_key
  ON sessions(host_client, host_pid, host_start)
  WHERE host_pid IS NOT NULL;

CREATE INDEX sessions_workspace ON sessions(root, tool);

CREATE TABLE aliases (
  alias            TEXT PRIMARY KEY,
  session_id       TEXT REFERENCES sessions(session_id),
  root             TEXT    NOT NULL,
  tool             TEXT    NOT NULL,
  lease_generation INTEGER NOT NULL DEFAULT 0, -- +1 on every successful claim
  updated_at       INTEGER NOT NULL
);

CREATE INDEX aliases_workspace ON aliases(root, tool);
CREATE INDEX aliases_session   ON aliases(session_id);

-- Single-row-per-key coordination table. The 'prune' row carries the gate that
-- keeps concurrent hooks from each deleting a batch.
CREATE TABLE meta (
  key              TEXT PRIMARY KEY,
  lease_token      TEXT,
  prune_started_at INTEGER,
  prune_done_at    INTEGER
);

INSERT INTO meta(key) VALUES ('prune');
`

// migrate brings an open database up to schemaVersion.
//
// The transaction is driven with explicit statements on a dedicated connection
// rather than through database/sql's Begin, because database/sql offers no way
// to ask for EXCLUSIVE and issuing BEGIN inside an already-begun Tx is an
// error.
//
// EXCLUSIVE is what makes concurrent first-run processes safe: the loser blocks
// on the write lock until the winner commits, then reads the already-bumped
// user_version and does nothing. Reading user_version before taking the lock
// would let both see 0 and both run the DDL.
func migrate(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration conn: %w", err)
	}
	defer conn.Close() //nolint:errcheck // returning the conn to the pool

	if _, err := conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return fmt.Errorf("acquire exclusive: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackTx(ctx, conn)
		}
	}()

	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	switch version {
	case schemaVersion: // already current — another process won the race
	case 0:
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
		// PRAGMA takes no bound parameters, hence the format; schemaVersion is
		// a compile-time constant so there is nothing to inject.
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("set user_version: %w", err)
		}
	default:
		return fmt.Errorf("database schema version %d is not supported by this binary (expected 0 or %d)", version, schemaVersion)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	committed = true
	return nil
}
