package deliver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/unghee/pager/internal/store"
)

// Listed is one row of `pager ls`.
type Listed struct {
	ID        int64
	Alias     string
	Sender    string
	Body      string
	Hop       int
	CreatedAt int64
	Delivered bool
	// Expired means the message is past the injection window: it is still
	// here, but it will not be delivered automatically again.
	Expired bool
}

// List returns the messages addressed to a session's inboxes.
//
// With expiredOnly it returns exactly what automatic delivery has given up on,
// which is the point of the flag: those messages are not lost, and listing them
// is how they get noticed and resent.
func List(ctx context.Context, st *store.Store, session string, expiredOnly bool, lim Limits) ([]Listed, error) {
	windowStart := st.Now() - lim.InjectTTL.Milliseconds()

	query := `
		SELECT m.id, m.alias, COALESCE(m.sender_label, ''), m.body, m.hop, m.created_at,
		       m.delivered_at IS NOT NULL, m.created_at < ?
		  FROM messages m JOIN aliases a ON a.alias = m.alias
		 WHERE a.session_id = ?`
	args := []any{windowStart, session}
	if expiredOnly {
		query += " AND m.delivered_at IS NULL AND m.created_at < ?"
		args = append(args, windowStart)
	}
	query += " ORDER BY m.id"

	rows, err := st.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var out []Listed
	for rows.Next() {
		var l Listed
		if err := rows.Scan(&l.ID, &l.Alias, &l.Sender, &l.Body, &l.Hop, &l.CreatedAt, &l.Delivered, &l.Expired); err != nil {
			return nil, fmt.Errorf("list messages: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SenderLabel is how a sender is shown to its recipient.
//
// An alias when it has one, its session id otherwise, and "human" only when
// there is no session at all. Falling back to "human" for a session that simply
// has no alias would be a lie the recipient acts on: --human is a meaningful
// claim in this system, and an agent's message must never wear it.
func SenderLabel(ctx context.Context, st *store.Store, session string) (string, error) {
	if session == "" {
		return "human", nil
	}
	alias, err := PrimaryAlias(ctx, st, session)
	if err != nil {
		return "", err
	}
	if alias != "" {
		return alias, nil
	}
	return session, nil
}

// RosterEntry is one session as `pager who` shows it.
type RosterEntry struct {
	SessionID string
	// Name is what the session is addressed by: the name it is known by, or
	// its session id when it has none yet. The fallback matches SenderLabel,
	// so the roster never shows an address that would not work.
	Name     string
	Tool     string
	Root     string
	LastSeen int64
}

// Roster lists the sessions a person could page right now, most recently
// active first.
//
// Stale sessions are left out rather than shown greyed: the roster exists to
// answer "who can I send this to", and a session whose hooks stopped running
// hours ago cannot be relied on to read anything.
func Roster(ctx context.Context, st *store.Store, staleAfter time.Duration) ([]RosterEntry, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT s.session_id,
		       COALESCE((SELECT a.alias FROM aliases a WHERE a.session_id = s.session_id
		                  `+primaryAliasOrder+` LIMIT 1), s.session_id),
		       s.tool, s.root, s.heartbeat_at
		  FROM sessions s
		 WHERE s.heartbeat_at >= ?
		 ORDER BY s.heartbeat_at DESC, s.session_id`,
		st.Now()-staleAfter.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []RosterEntry
	for rows.Next() {
		var e RosterEntry
		if err := rows.Scan(&e.SessionID, &e.Name, &e.Tool, &e.Root, &e.LastSeen); err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// primaryAliasOrder ranks the names a session holds: the most recently set one
// is the one it is known by.
//
// That rule is what lets an automatic name be a plain alias row. A session is
// named when it first runs a hook, and any name a person sets afterwards is
// newer, so it wins without the schema having to record which name was chosen
// by whom. SetAlias and ClaimAlias therefore guarantee a strictly increasing
// timestamp rather than merely writing the clock — see their statements.
const primaryAliasOrder = "ORDER BY updated_at DESC, alias"

// PrimaryAlias returns the alias a session is best known by, or "" when it has
// none.
func PrimaryAlias(ctx context.Context, st *store.Store, session string) (string, error) {
	var alias string
	err := st.DB().QueryRowContext(ctx,
		"SELECT alias FROM aliases WHERE session_id = ? "+primaryAliasOrder+" LIMIT 1", session).Scan(&alias)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read primary alias: %w", err)
	}
	return alias, nil
}
