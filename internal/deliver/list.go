package deliver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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

// PrimaryAlias returns the alias a session is best known by, or "" when it has
// none. It is how a send labels itself so the recipient sees a name rather than
// a session id.
func PrimaryAlias(ctx context.Context, st *store.Store, session string) (string, error) {
	var alias string
	err := st.DB().QueryRowContext(ctx,
		"SELECT alias FROM aliases WHERE session_id = ? ORDER BY alias LIMIT 1", session).Scan(&alias)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read primary alias: %w", err)
	}
	return alias, nil
}
