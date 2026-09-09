package deliver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/iMUngHee/pager/internal/store"
)

// Limits are the delivery bounds. Every default is an estimate; no production
// signal informed them.
type Limits struct {
	// MaxInjectBytes caps the UTF-8 size of the injected text itself, senders
	// and ids and separators included — not the sum of the message bodies.
	MaxInjectBytes int
	// MaxBatch caps how many messages one run may carry. It and the byte
	// budget both apply; whichever binds first wins.
	MaxBatch int
	// MaxBodyRunes caps one body, in characters rather than bytes so a
	// Korean message is not cut three times shorter than an English one.
	MaxBodyRunes int
	// InjectTTL is how long a message stays eligible for automatic delivery.
	// Past it the message survives in the database but stops being retried,
	// which is what keeps an abandoned session from retrying forever.
	InjectTTL time.Duration
	// Lease is how long a claim holds a message before another run may take
	// it. It only has to outlast writing to stdout and confirming.
	Lease time.Duration
}

// DefaultLimits returns the built-in bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxInjectBytes: 2000,
		MaxBatch:       5,
		MaxBodyRunes:   280,
		InjectTTL:      72 * time.Hour,
		Lease:          2 * time.Minute,
	}
}

// LimitsFromEnv applies the PAGER_* overrides to the defaults. A malformed or
// non-positive value is ignored rather than fatal: this runs inside a hook,
// where refusing to start would cost the session its delivery entirely.
func LimitsFromEnv() Limits {
	lim := DefaultLimits()
	intVar("PAGER_MAX_INJECT_BYTES", &lim.MaxInjectBytes)
	intVar("PAGER_MAX_BATCH", &lim.MaxBatch)
	intVar("PAGER_MAX_BODY_RUNES", &lim.MaxBodyRunes)
	durationVar("PAGER_INJECT_TTL", &lim.InjectTTL)
	durationVar("PAGER_LEASE", &lim.Lease)
	return lim
}

func intVar(name string, target *int) {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		*target = v
	}
}

func durationVar(name string, target *time.Duration) {
	if v, err := time.ParseDuration(os.Getenv(name)); err == nil && v > 0 {
		*target = v
	}
}

// Pending is one message eligible for injection.
type Pending struct {
	ID        int64
	Sender    string
	Body      string
	Hop       int
	CreatedAt int64
	// OriginalRunes is the body's length before truncation, or 0 when it was
	// delivered whole. The recipient is told, so a cut never reads as the
	// whole message.
	OriginalRunes int
}

// Batch is what one hook run will write out.
type Batch struct {
	Token    string
	Messages []Pending
	// Held is how many eligible messages did not fit this run. They were
	// deliberately left unclaimed so the next run can take them immediately.
	Held int
	// Expired is how many are past the injection window and will not be
	// delivered automatically at all.
	Expired int
}

// Empty reports whether there is nothing to say.
func (b Batch) Empty() bool { return len(b.Messages) == 0 }

// CollectBatch gathers what a session should be shown next.
//
// The order matters and is the reason claiming is not folded into the query.
// Candidates are read without claiming, the budget decides which of them fit,
// and only those are claimed. Claiming first and discarding the overflow would
// leave the discarded messages held by a lease nobody intends to honour, so
// they would reach no one until it expired.
func CollectBatch(ctx context.Context, st *store.Store, session string, lim Limits) (Batch, error) {
	candidates, expired, err := Candidates(ctx, st, session, lim)
	if err != nil {
		return Batch{}, err
	}

	chosen, held := selectWithinBudget(candidates, expired, lim)
	batch := Batch{Messages: chosen, Held: held, Expired: expired}
	if len(chosen) == 0 {
		return batch, nil
	}

	ids := make([]int64, len(chosen))
	for i, m := range chosen {
		ids[i] = m.ID
	}
	token, claimed, err := Claim(ctx, st, session, ids, lim)
	if err != nil {
		return Batch{}, err
	}
	// Re-validation inside the claim may have dropped some, so report what was
	// actually reserved rather than what was hoped for.
	batch.Token = token
	batch.Messages = filterByID(chosen, claimed)
	batch.Held = held + (len(chosen) - len(batch.Messages))
	return batch, nil
}

// Candidates lists a session's deliverable messages without claiming any, and
// counts those that have outlived the injection window.
func Candidates(ctx context.Context, st *store.Store, session string, lim Limits) ([]Pending, int, error) {
	now := st.Now()
	windowStart := now - lim.InjectTTL.Milliseconds()

	rows, err := st.DB().QueryContext(ctx, `
		SELECT m.id, COALESCE(m.sender_label, ''), m.body, m.hop, m.created_at
		  FROM messages m JOIN aliases a ON a.alias = m.alias
		 WHERE a.session_id = ?
		   AND m.delivered_at IS NULL
		   AND m.created_at >= ?
		   AND (m.claim_expires_at IS NULL OR m.claim_expires_at < ?)
		 ORDER BY m.id`, session, windowStart, now)
	if err != nil {
		return nil, 0, fmt.Errorf("list candidates: %w", err)
	}
	defer rows.Close()

	var out []Pending
	for rows.Next() {
		var p Pending
		if err := rows.Scan(&p.ID, &p.Sender, &p.Body, &p.Hop, &p.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("list candidates: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list candidates: %w", err)
	}

	// This count is not part of the delivery decision -- the candidate SELECT
	// above is, and it deliberately ignores listed_at so a polled message is
	// re-injected. This one exists to be rendered, and renderTail spends it on a
	// sentence that names `pager ls --expired`. So it has to be counted the way
	// that command counts: a message an agent has already read by polling needs
	// no resending, and reporting it here would point the reader at a listing
	// shorter than the number they were just given.
	//
	// It does reach selectWithinBudget, which measures Render(trial) against the
	// byte budget, so the count influences how many messages fit in one batch.
	// That is bounded and one-directional: this predicate is strictly narrower
	// than the old one, so the count never rises, the tail never grows, and a
	// batch never shrinks.
	var expired int
	if err := st.DB().QueryRowContext(ctx, `
		SELECT count(*) FROM messages m JOIN aliases a ON a.alias = m.alias
		 WHERE a.session_id = ? AND `+undealtWith+` AND m.created_at < ?`,
		session, windowStart).Scan(&expired); err != nil {
		return nil, 0, fmt.Errorf("count expired: %w", err)
	}
	return out, expired, nil
}

// Claim reserves messages for one run, returning the claim token and the ids
// that were actually reserved.
//
// Everything happens under one write lock, in this order: re-validate the
// selection, reserve a contiguous range from the session's sequence cursor, and
// stamp the messages. SQLite does not allow DML inside a CTE, so this cannot be
// one statement — but it must be one transaction, or a failure partway through
// would leave the cursor advanced over sequence numbers nothing ever received.
//
// Re-validation is not paranoia. The candidate query deliberately ran outside a
// transaction, so in between another run may have claimed the same messages,
// confirmed them, or taken over the alias they are addressed to.
func Claim(ctx context.Context, st *store.Store, session string, ids []int64, lim Limits) (string, []int64, error) {
	if len(ids) == 0 {
		return "", nil, nil
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", nil, fmt.Errorf("claim token: %w", err)
	}
	token := hex.EncodeToString(buf[:])
	now := st.Now()

	var reserved []int64
	err := st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		reserved = nil
		valid, err := revalidate(ctx, c, session, ids, now, lim)
		if err != nil {
			return err
		}
		if len(valid) == 0 {
			return nil
		}

		// The increased value is the top of the reserved range, so the range is
		// (base-n, base]. Two runs reserving at once get disjoint ranges
		// because the update is atomic.
		var base int64
		if err := c.QueryRowContext(ctx,
			"UPDATE sessions SET seq_cursor = seq_cursor + ? WHERE session_id = ? RETURNING seq_cursor",
			len(valid), session).Scan(&base); err != nil {
			return fmt.Errorf("reserve sequence range: %w", err)
		}

		var epoch int64
		if err := c.QueryRowContext(ctx,
			"SELECT causal_epoch FROM sessions WHERE session_id = ?", session).Scan(&epoch); err != nil {
			return fmt.Errorf("read causal epoch: %w", err)
		}

		for i, id := range valid {
			seq := base - int64(len(valid)) + int64(i) + 1
			if _, err := c.ExecContext(ctx, `
				UPDATE messages
				   SET claim_token = ?, claim_epoch = ?, delivery_seq = ?,
				       claimed_at = ?, claim_expires_at = ?
				 WHERE id = ?`,
				token, epoch, seq, now, now+lim.Lease.Milliseconds(), id); err != nil {
				return fmt.Errorf("stamp claim: %w", err)
			}
		}
		reserved = valid
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	if len(reserved) == 0 {
		return "", nil, nil
	}
	return token, reserved, nil
}

// revalidate returns, in id order, the subset of ids still deliverable to this
// session. Ordering by id makes a batch's sequence assignment deterministic,
// which is what lets a replay reproduce the same chain.
func revalidate(ctx context.Context, c *sql.Conn, session string, ids []int64, now int64, lim Limits) ([]int64, error) {
	args := make([]any, 0, len(ids)+3)
	args = append(args, session, now-lim.InjectTTL.Milliseconds(), now)
	for _, id := range ids {
		args = append(args, id)
	}

	query := `
		SELECT m.id FROM messages m JOIN aliases a ON a.alias = m.alias
		 WHERE a.session_id = ?
		   AND m.delivered_at IS NULL
		   AND m.created_at >= ?
		   AND (m.claim_expires_at IS NULL OR m.claim_expires_at < ?)
		   AND m.id IN (` + placeholders(len(ids)) + `)
		 ORDER BY m.id`

	rows, err := c.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("re-validate claim: %w", err)
	}
	defer rows.Close()

	var valid []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("re-validate claim: %w", err)
		}
		valid = append(valid, id)
	}
	return valid, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func filterByID(all []Pending, keep []int64) []Pending {
	set := make(map[int64]struct{}, len(keep))
	for _, id := range keep {
		set[id] = struct{}{}
	}
	out := make([]Pending, 0, len(keep))
	for _, m := range all {
		if _, ok := set[m.ID]; ok {
			out = append(out, m)
		}
	}
	return out
}
