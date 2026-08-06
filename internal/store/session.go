package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DefaultStale is how long a session may go without a hook event before other
// sessions may treat it as gone. Estimated, not measured.
const DefaultStale = 12 * time.Hour

// SessionRecord is what a hook knows about its own session.
type SessionRecord struct {
	ID    string
	Tool  string
	Root  string
	PMRef string // optional KEY/id convenience; empty leaves any existing value

	// HostClient, HostPid and HostStart bind the session to its host process.
	// A zero HostPid means detection failed, in which case any existing
	// binding is left alone rather than cleared — a hook that briefly cannot
	// see its ancestry should not cost the session its attribution.
	HostClient string
	HostPid    int
	HostStart  int64
}

// RecordSession upserts the calling session and refreshes its heartbeat.
//
// This is the write half of the session-binding contract: hooks record, the CLI
// and the MCP server look up.
func (s *Store) RecordSession(ctx context.Context, rec SessionRecord) error {
	if rec.ID == "" {
		return errors.New("record session: empty session id")
	}
	var pmRef any
	if rec.PMRef != "" {
		pmRef = rec.PMRef
	}
	now := s.now()

	return s.writeTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		if rec.HostPid <= 1 {
			_, err := c.ExecContext(ctx, `
				INSERT INTO sessions(session_id, tool, root, pm_ref, heartbeat_at)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(session_id) DO UPDATE SET
					tool         = excluded.tool,
					root         = excluded.root,
					pm_ref       = COALESCE(excluded.pm_ref, sessions.pm_ref),
					heartbeat_at = excluded.heartbeat_at`,
				rec.ID, rec.Tool, rec.Root, pmRef, now)
			if err != nil {
				return fmt.Errorf("record session: %w", err)
			}
			return nil
		}

		// Take the host key from whoever held it. A /clear or a resume gives
		// the same host process a new session id, and the unique index on the
		// host key would reject the upsert if the previous row kept it. Doing
		// this in the same transaction as the upsert is what keeps a reader
		// from ever observing two rows — or none — for one host.
		if _, err := c.ExecContext(ctx, `
			UPDATE sessions SET host_client = NULL, host_pid = NULL, host_start = NULL
			 WHERE host_client = ? AND host_pid = ? AND host_start = ? AND session_id <> ?`,
			rec.HostClient, rec.HostPid, rec.HostStart, rec.ID); err != nil {
			return fmt.Errorf("release host key: %w", err)
		}
		if _, err := c.ExecContext(ctx, `
			INSERT INTO sessions(session_id, tool, root, pm_ref, heartbeat_at, host_client, host_pid, host_start)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET
				tool         = excluded.tool,
				root         = excluded.root,
				pm_ref       = COALESCE(excluded.pm_ref, sessions.pm_ref),
				heartbeat_at = excluded.heartbeat_at,
				host_client  = excluded.host_client,
				host_pid     = excluded.host_pid,
				host_start   = excluded.host_start`,
			rec.ID, rec.Tool, rec.Root, pmRef, now, rec.HostClient, rec.HostPid, rec.HostStart); err != nil {
			return fmt.Errorf("record session: %w", err)
		}
		return nil
	})
}

// SessionByHost returns the session bound to a host key whose heartbeat is
// still fresh, or "" when there is none.
//
// A missing row is the normal outcome, not an error: it simply means this
// invocation is unattributed. The unique index on the host key is what lets
// this be a single-row query — without it a caller could be attributed to an
// arbitrary one of several matching sessions.
func (s *Store) SessionByHost(ctx context.Context, client string, pid int, start int64, staleAfter time.Duration) (string, error) {
	if pid <= 1 {
		return "", nil
	}
	var id string
	err := s.db.QueryRowContext(ctx, `
		SELECT session_id FROM sessions
		 WHERE host_client = ? AND host_pid = ? AND host_start = ? AND heartbeat_at >= ?`,
		client, pid, start, s.now()-staleAfter.Milliseconds()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup session by host: %w", err)
	}
	return id, nil
}
