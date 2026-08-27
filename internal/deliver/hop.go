package deliver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/unghee/pager/internal/store"
)

// MaxHop is the deepest causal chain pager will carry. A reply to a reply to a
// reply is already past the point where two agents are talking rather than
// working.
const MaxHop = 3

// Message origin values. The column survives its cause being pruned, so the
// distinction outlives cause_id.
const (
	OriginHuman  = "human"
	OriginCaused = "caused"
)

// Circuit-breaker limits. These are the last line of defence, not the first:
// --human is an operator assertion rather than a verified fact, so an automated
// caller can always claim human origin and step outside the hop chain. What it
// cannot step outside is a count.
//
// The values are estimates; no production signal informed them.
const (
	breakerWindow      = time.Hour
	maxCausedPerSender = 20
	maxCausedPerPair   = 10
	maxHumanPerWindow  = 30
)

// Send refusals. Each is a policy decision, not a failure.
var (
	// ErrUnattributed is the fail-closed default: a send that cannot say which
	// session it came from is refused rather than recorded anonymously.
	ErrUnattributed = errors.New("no session context: pass --session, set PAGER_SESSION, or assert --human")
	// ErrHopExceeded means the causal chain reached MaxHop. What it wraps names
	// the remedy, because the reader is usually an agent deciding what to do
	// next — see the message built at the refusal.
	ErrHopExceeded = errors.New("causal chain is too deep")
	// ErrBreakerOpen means a rate limit tripped.
	ErrBreakerOpen = errors.New("send rate limit reached")
)

// SendRequest describes one outgoing message.
//
// There is deliberately no cause field. The cause is whatever the sending
// session most recently took delivery of, read inside the same transaction that
// writes the message, so a caller cannot understate its own depth.
type SendRequest struct {
	Alias  string // resolved target alias
	Body   string
	Sender string // sending session; empty when unattributed
	Label  string // how the recipient sees the sender
	Human  bool   // operator assertion: send as human-origin, breaking the chain
}

// Sent reports what was written.
type Sent struct {
	ID     int64
	Hop    int
	Origin string
}

// Send writes one message, computing its causal depth and enforcing the limits
// in the same transaction that inserts it.
//
// Reading the cause, deciding the depth, counting against the breaker and
// inserting all happen under one write lock. Split apart, two concurrent sends
// could each read a count of 19 and both proceed.
func Send(ctx context.Context, st *store.Store, req SendRequest) (Sent, error) {
	if strings.TrimSpace(req.Alias) == "" {
		return Sent{}, errors.New("send: no target alias")
	}
	if !req.Human && req.Sender == "" {
		return Sent{}, ErrUnattributed
	}

	var out Sent
	err := st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		hop, origin, cause, err := causalDepth(ctx, c, req)
		if err != nil {
			return err
		}
		if hop > MaxHop {
			// The remedy is spelled out because the reader is almost always an
			// agent, and the count alone does not tell it what to do. Read as
			// bare numbers this refusal looks like a bug in ordinary
			// discussion, and it was reported as one: a session hit it, and
			// told its operator pager was cutting normal technical exchange
			// short. It is not — what is capped is a chain of agent replies
			// with no person in it, which is the whole point of MaxHop, and the
			// operator being told is the correct outcome rather than the
			// symptom. Saying so is what turns a refusal into an instruction.
			return fmt.Errorf(
				"%w: hop %d exceeds the limit of %d — that many replies have passed with no person in them; "+
					"report it to your user rather than retrying, since their next prompt starts a fresh chain, "+
					"and --human is an operator's assertion rather than a way past this",
				ErrHopExceeded, hop, MaxHop)
		}
		if err := checkBreaker(ctx, c, st.Now(), req, origin); err != nil {
			return err
		}

		res, err := c.ExecContext(ctx, `
			INSERT INTO messages(alias, sender_session, sender_label, body, hop, origin, cause_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			req.Alias, nullable(req.Sender), req.Label, req.Body, hop, origin, cause, st.Now())
		if err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
		out = Sent{ID: id, Hop: hop, Origin: origin}
		return nil
	})
	return out, err
}

// causalDepth decides how deep this send sits in a chain.
//
// A send is caused when the sending session is currently holding a delivered
// message as its causal representative. With none — either the session has
// received nothing this epoch, or a user prompt reset it — the send starts a
// fresh chain at zero. That reset is what keeps ordinary back-and-forth work
// from accumulating depth forever.
//
// When the recorded cause can no longer be read because retention removed it,
// the depth is treated as beyond the limit rather than as zero. Otherwise
// waiting for a prune would launder a chain back to the start.
func causalDepth(ctx context.Context, c *sql.Conn, req SendRequest) (hop int, origin string, cause any, err error) {
	if req.Human {
		return 0, OriginHuman, nil, nil
	}

	var causeID sql.NullInt64
	err = c.QueryRowContext(ctx,
		"SELECT last_inbound_id FROM sessions WHERE session_id = ?", req.Sender).Scan(&causeID)
	if errors.Is(err, sql.ErrNoRows) {
		// The sender never attached. Treat it as having no context at all
		// rather than as a fresh chain.
		return 0, "", nil, ErrUnattributed
	}
	if err != nil {
		return 0, "", nil, fmt.Errorf("read causal state: %w", err)
	}
	if !causeID.Valid {
		return 0, OriginHuman, nil, nil
	}

	var causeHop int
	err = c.QueryRowContext(ctx, "SELECT hop FROM messages WHERE id = ?", causeID.Int64).Scan(&causeHop)
	if errors.Is(err, sql.ErrNoRows) {
		return MaxHop + 1, OriginCaused, causeID.Int64, nil
	}
	if err != nil {
		return 0, "", nil, fmt.Errorf("read cause depth: %w", err)
	}
	return causeHop + 1, OriginCaused, causeID.Int64, nil
}

// checkBreaker enforces the three send limits.
//
// Automatic traffic is bounded twice — per sender and per sender-recipient pair
// — because one runaway conversation between two sessions is the failure mode,
// and a per-sender limit alone would let it consume the whole budget.
func checkBreaker(ctx context.Context, c *sql.Conn, now int64, req SendRequest, origin string) error {
	since := now - breakerWindow.Milliseconds()

	if origin == OriginCaused {
		var perSender int
		if err := c.QueryRowContext(ctx, `
			SELECT count(*) FROM messages
			 WHERE sender_session = ? AND cause_id IS NOT NULL AND created_at >= ?`,
			req.Sender, since).Scan(&perSender); err != nil {
			return fmt.Errorf("count automatic sends: %w", err)
		}
		if perSender >= maxCausedPerSender {
			return fmt.Errorf("%w: %d automatic sends from this session in the last hour (limit %d)",
				ErrBreakerOpen, perSender, maxCausedPerSender)
		}

		var perPair int
		if err := c.QueryRowContext(ctx, `
			SELECT count(*) FROM messages
			 WHERE sender_session = ? AND alias = ? AND cause_id IS NOT NULL AND created_at >= ?`,
			req.Sender, req.Alias, since).Scan(&perPair); err != nil {
			return fmt.Errorf("count automatic sends to this alias: %w", err)
		}
		if perPair >= maxCausedPerPair {
			return fmt.Errorf("%w: %d automatic sends to %q in the last hour (limit %d)",
				ErrBreakerOpen, perPair, req.Alias, maxCausedPerPair)
		}
		return nil
	}

	// Everything without a cause shares one budget, including a session's own
	// spontaneous sends. Such a send cannot itself be part of a loop — the
	// reply it provokes is what carries a cause — so a coarse cap is enough.
	var human int
	if err := c.QueryRowContext(ctx, `
		SELECT count(*) FROM messages WHERE cause_id IS NULL AND created_at >= ?`,
		since).Scan(&human); err != nil {
		return fmt.Errorf("count human sends: %w", err)
	}
	if human >= maxHumanPerWindow {
		return fmt.Errorf("%w: %d human-origin sends in the last hour (limit %d)",
			ErrBreakerOpen, human, maxHumanPerWindow)
	}
	return nil
}

// Confirmation is the outcome of confirming one claimed batch.
type Confirmation struct {
	Delivered   int64 // messages marked delivered
	CausalMoved bool  // whether the recipient's causal representative advanced
}

// ConfirmDelivery marks every message claimed under token as delivered and,
// when the claim is still causally current, advances the session's causal
// representative.
//
// The representative is the batch's deepest message, ties broken by id. Picking
// a shallower one would let a chain regress: the session would answer a hop-3
// message at depth 1 and the ping-pong guard would never engage. The tiebreak
// keeps a replay deterministic.
//
// Two conditions guard the causal update, and they catch different accidents.
// The epoch condition rejects a confirmation that was claimed before a user
// prompt reset the chain — that message went out, but the user has since taken
// the conversation somewhere else, so it has no business becoming the cause of
// what follows. The sequence condition rejects an out-of-order confirmation
// within one epoch.
//
// Notably hop takes no part in either. Depth is not arrival order: an earlier
// design compared (hop, id) and so refused to record a legitimate new chain
// starting at zero after a deep one had ended, which left the session computing
// its next send at hop 4 forever.
//
// When the causal update is rejected the messages are still marked delivered.
// They have already been written to the recipient; redelivering them would
// duplicate the instruction. The result is a message that was shown but caused
// nothing, which is the intended outcome.
func ConfirmDelivery(ctx context.Context, st *store.Store, session, token string) (Confirmation, error) {
	var out Confirmation
	err := st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		var repID, repSeq, epoch int64
		err := c.QueryRowContext(ctx, `
			SELECT id, COALESCE(delivery_seq, 0), COALESCE(claim_epoch, 0) FROM messages
			 WHERE claim_token = ? AND delivered_at IS NULL
			 ORDER BY hop DESC, id DESC
			 LIMIT 1`, token).Scan(&repID, &repSeq, &epoch)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // nothing outstanding under this token
		}
		if err != nil {
			return fmt.Errorf("read claimed batch: %w", err)
		}

		res, err := c.ExecContext(ctx,
			"UPDATE messages SET delivered_at = ? WHERE claim_token = ? AND delivered_at IS NULL",
			st.Now(), token)
		if err != nil {
			return fmt.Errorf("mark delivered: %w", err)
		}
		if out.Delivered, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("mark delivered: %w", err)
		}

		res, err = c.ExecContext(ctx, `
			UPDATE sessions
			   SET last_inbound_id = ?, last_inbound_seq = ?
			 WHERE session_id = ?
			   AND causal_epoch     = ?   -- not superseded by a user prompt
			   AND last_inbound_seq < ?`, // not an out-of-order confirmation
			repID, repSeq, session, epoch, repSeq)
		if err != nil {
			return fmt.Errorf("advance causal state: %w", err)
		}
		moved, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("advance causal state: %w", err)
		}
		out.CausalMoved = moved > 0
		return nil
	})
	return out, err
}

// ResetCausal breaks the causal chain for a session, which a structurally
// verified user prompt does.
//
// Bumping the epoch is what invalidates confirmations that were already in
// flight; clearing the representative is what makes the session's next send
// start a fresh chain at zero.
func ResetCausal(ctx context.Context, st *store.Store, session string) error {
	_, err := st.Exec(ctx, `
		UPDATE sessions
		   SET causal_epoch = causal_epoch + 1, last_inbound_id = NULL, last_inbound_seq = 0
		 WHERE session_id = ?`, session)
	if err != nil {
		return fmt.Errorf("reset causal state: %w", err)
	}
	return nil
}

// nullable turns an empty string into a SQL NULL, so an unattributed sender is
// stored as absent rather than as "".
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
