package deliver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/iMUngHee/pager/internal/store"
)

// Presence is what a sender is told about whoever is behind the name it just
// addressed.
//
// The vocabulary lives here rather than at each send path for the reason
// Listed.State's comment gives, with the drift already on the record: the CLI
// and the MCP server each carried their own sentence for the no-holder case
// ("has no live session — it will be delivered when one claims the alias"
// against "no live session holds that alias yet; it will be delivered when one
// claims it"), same fact worded twice. One sentence is what stops a third.
//
// This answers a different question from wake. Wake reports whether a poke
// landed, and its Queued covers both a dead process and a live session whose
// host offers no surface to poke — a codex session that has not run a turn is
// unreachable and perfectly alive. Reachability and liveness are orthogonal, so
// neither stands in for the other.
type Presence int

const (
	// PresenceUnheld means no session holds the alias at all.
	//
	// No code path produces this today, and the branch is kept for the state
	// rather than the event: PresenceOf has nothing to probe when a target
	// carries no session id, so the case must be answered whether or not it
	// occurs. Both alias INSERTs take session_id from the sessions primary key,
	// ClaimAlias only ever writes a non-empty session, nothing sets the column
	// to NULL, and nothing deletes from sessions or aliases — prune touches
	// messages only. Its test builds the row with direct SQL and says so; do
	// not read that test as evidence this happens.
	PresenceUnheld Presence = iota

	// PresenceStranded means a session holds the alias and its host process is
	// gone. This is the case the whole type exists for: the name resolves, the
	// send succeeds, and nothing will read it.
	PresenceStranded

	// PresenceHeld means a session holds the alias and may still be there.
	//
	// "May" is deliberate. An unrecorded or unknowable host lands here rather
	// than in Stranded, because nothing has been learned about it and claiming
	// nobody is home on no evidence hides a live notification. Only a probe
	// that actually answered can move a target out of this state.
	PresenceHeld
)

// HostProbe answers whether a recorded host process is still the one running.
//
// It is injected rather than called inside this package for two reasons that
// point the same way. A function that probes the environment itself has a
// branch no test can reach — proving PresenceHeld needs a process genuinely
// running under an exact recorded start token, which a caller can hand over and
// a unit test cannot conjure. And deliver reads the database, never process
// state; host_pid and host_start are opaque integers here.
//
// sessionref.AliveAt has this shape.
type HostProbe func(pid int, start int64) (alive, known bool)

// PresenceOf reports what is behind a resolved target.
//
// A missing sessions row is treated as Unheld rather than as an error: the
// column is a nullable reference, so a target naming a session that is not
// there is the same absence as a target naming none.
func PresenceOf(ctx context.Context, st *store.Store, t Target, probe HostProbe) (Presence, error) {
	if t.SessionID == "" {
		return PresenceUnheld, nil
	}

	var pid sql.NullInt64
	var start sql.NullInt64
	err := st.DB().QueryRowContext(ctx,
		"SELECT host_pid, host_start FROM sessions WHERE session_id = ?", t.SessionID).Scan(&pid, &start)
	if errors.Is(err, sql.ErrNoRows) {
		return PresenceUnheld, nil
	}
	if err != nil {
		return PresenceHeld, fmt.Errorf("read host of %s: %w", t.SessionID, err)
	}

	// A null pid is detection having failed at attach time, which the probe
	// reports as unknowable anyway once it is handed a zero.
	alive, known := probe(int(pid.Int64), start.Int64)
	if known && !alive {
		return PresenceStranded, nil
	}
	return PresenceHeld, nil
}

// Note is the line a send prints about this presence, or "" when there is
// nothing to add.
//
// Held says nothing because wake speaks for it, and two lines about the same
// recipient would leave a reader deciding which one to believe.
//
// The stranded sentence carries three facts and drops none of them under a
// reword. Nobody is behind the name — which the sender cannot otherwise tell,
// since the name resolved and the send succeeded. The message is kept, not
// lost. And the way it ever gets read is a claim from that alias's own
// workspace, because ClaimAlias matches on root and tool, so a session
// elsewhere cannot take it however willing. pager who is named because the
// sender's real next move is usually to address someone who can answer now.
func (p Presence) Note(alias string) string {
	switch p {
	case PresenceUnheld:
		return "note: nothing holds " + alias + " — the message waits until a session claims that name"
	case PresenceStranded:
		return "note: " + alias + "'s session is gone — the message waits, but nothing will read it " +
			"until a session in that workspace claims the name; pager who lists who can answer now"
	default:
		return ""
	}
}
