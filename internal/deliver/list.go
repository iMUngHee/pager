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
	// Delivered means a hook has injected this message into its recipient's
	// context.
	Delivered bool
	// Seen means an agent pulled this message up in a listing of its own inbox.
	//
	// It is the other axis of having been read, and the two are independent:
	// Delivered is what a hook pushed into a context, Seen is what an agent went
	// and looked at. A message can be either, both, or neither.
	Seen bool
	// Expired means the message is past the injection window: it is still
	// here, but it will not be delivered automatically again.
	Expired bool
}

// State is the one word a listing prints for this message.
//
// The vocabulary lives here rather than in each caller because the flag names
// are derived from it: `--waiting` and `--expired` are meant to be the value a
// person just read in the STATE column. Two copies of this switch would let the
// CLI and the MCP tool drift apart, and a drift is a broken promise rather than
// a cosmetic difference.
//
// The order is the strength of the fact. delivered wins over seen because it is
// the stronger claim — the message reached a context, not merely a listing — so
// a row that is both prints delivered and the poll is only visible in the
// column. That loses nothing a filter needs: both narrowing filters exclude the
// row on either axis alone. Whether anyone has dealt with the message is the
// question the labels answer, and both of the first two mean yes.
func (l Listed) State() string {
	switch {
	case l.Delivered:
		return "delivered"
	case l.Seen:
		return "seen"
	case l.Expired:
		return "expired"
	default:
		return "waiting"
	}
}

// Filter narrows what List returns.
//
// The three values nest rather than partition: an expired message is one that
// is still waiting and has also aged out, so expired ⊂ waiting ⊂ all. That is
// why this is one value and not two booleans — there is no combination for the
// query to reconcile, only a choice of how far to narrow.
type Filter int

const (
	// FilterAll returns every message addressed to the session, delivered ones
	// included.
	FilterAll Filter = iota
	// FilterWaiting returns only what nobody has dealt with yet — neither
	// injected by a hook nor looked at in a listing. This is what a poll during a
	// long turn wants: the answer is usually empty, and the cost of asking should
	// match what it finds rather than what has accumulated.
	FilterWaiting
	// FilterExpired returns exactly what automatic delivery has given up on and
	// nobody has since looked at: those messages are not lost, and listing them
	// is how they get noticed and resent. One that has been read by a poll needs
	// no resending, so it is not here.
	FilterExpired
)

// undealtWith is what everything that says "waiting" means by it.
//
// The two axes are independent — delivered_at is set only by a hook confirming
// an injection, listed_at only by an agent pulling the message up in a listing
// of its own inbox — so neither alone answers "has anyone dealt with this". Both
// filters share this one clause rather than spelling it twice, because the
// sharing IS the nesting invariant the Filter doc above states: narrow expired
// with the same condition and expired ⊂ waiting holds by construction.
//
// Inboxes counts with it too, which is why it is a bare predicate rather than a
// clause carrying its own " AND ": there it is the first condition in the WHERE.
// Every caller therefore joins it explicitly. A whole-store count that drifted
// from `ls --waiting` would be two answers to one question.
const undealtWith = "m.delivered_at IS NULL AND m.listed_at IS NULL"

// FilterFrom resolves the two narrowing flags the CLI and the MCP server each
// expose.
//
// Expired wins when both are set. The two are not in conflict — expired is the
// intersection — so applying the narrower one is what honours both, and there
// is nothing to reject.
func FilterFrom(waiting, expired bool) Filter {
	switch {
	case expired:
		return FilterExpired
	case waiting:
		return FilterWaiting
	}
	return FilterAll
}

// List returns the messages addressed to a session's inboxes, narrowed by f.
func List(ctx context.Context, st *store.Store, session string, f Filter, lim Limits) ([]Listed, error) {
	windowStart := st.Now() - lim.InjectTTL.Milliseconds()

	query := `
		SELECT m.id, m.alias, COALESCE(m.sender_label, ''), m.body, m.hop, m.created_at,
		       m.delivered_at IS NOT NULL, m.listed_at IS NOT NULL, m.created_at < ?
		  FROM messages m JOIN aliases a ON a.alias = m.alias
		 WHERE a.session_id = ?`
	args := []any{windowStart, session}
	switch f {
	case FilterWaiting:
		query += " AND " + undealtWith
	case FilterExpired:
		query += " AND " + undealtWith + " AND m.created_at < ?"
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
		if err := rows.Scan(&l.ID, &l.Alias, &l.Sender, &l.Body, &l.Hop, &l.CreatedAt,
			&l.Delivered, &l.Seen, &l.Expired); err != nil {
			return nil, fmt.Errorf("list messages: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MarkListed records that an agent pulled these messages up in a listing of its
// own inbox, and reports how many rows that changed.
//
// Only the MCP handler calls this, and the boundary is deliberate rather than a
// convention to remember: `pager ls --session <id>` can list another session's
// inbox, so marking from the shared listing path would let a person glancing at
// a queue consume an agent's mail. msg_list has no target argument at all — it
// resolves its own session — so the MCP path can only ever stamp its own inbox.
// List therefore stays a pure read.
//
// Two clauses shape what gets written. listed_at IS NULL keeps the first
// sighting rather than the latest: the meaningful fact is when a message first
// came into view, and keeping it makes the column monotone. delivered_at IS NULL
// gives the column one crisp meaning — listed_at is set exactly when an agent
// looked at a row that was still undelivered — and a row already delivered when
// it was listed has no reader for the fact, since both narrowing filters exclude
// it on delivered_at alone.
//
// Note what this does NOT promise: a row stamped here can still be delivered
// afterwards, because the hook's candidate selection deliberately ignores
// listed_at so a polled message is re-injected in case the context compacted.
// Such a row carries both columns, and archives it with both.
func MarkListed(ctx context.Context, st *store.Store, ids []int64) (int64, error) {
	// SQLite accepts IN () and matches nothing, so this guard changes no
	// observable behaviour — measured, not assumed. It is here to skip a round
	// trip on the path this whole column exists for: a poll during a long turn
	// whose inbox is already fully stamped. Claim guards the same way, so a
	// caller does not have to know which of the two it is calling.
	if len(ids) == 0 {
		return 0, nil
	}

	args := make([]any, 0, len(ids)+1)
	args = append(args, st.Now())
	for _, id := range ids {
		args = append(args, id)
	}

	res, err := st.Exec(ctx, `
		UPDATE messages SET listed_at = ?
		 WHERE id IN (`+placeholders(len(ids))+`)
		   AND listed_at IS NULL
		   AND delivered_at IS NULL`, args...)
	if err != nil {
		return 0, fmt.Errorf("mark listed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("mark listed: %w", err)
	}
	return n, nil
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
