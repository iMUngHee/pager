// Package deliver implements pager's delivery rules: how a reference resolves
// to an inbox, who may take over an alias, how causal depth is computed, and
// which pending messages a hook may claim.
//
// Most of the correctness here lives in exact SQL conditions rather than in Go
// control flow. A claim that checks staleness before updating instead of inside
// the update is a race, not a style preference, so the statements live beside
// the rules they enforce.
package deliver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iMUngHee/pager/internal/store"
)

// Target is a resolved delivery address. Messages are addressed to an alias;
// SessionID is empty when the alias exists but no session currently holds it,
// which is exactly the orphan case.
type Target struct {
	Alias     string
	SessionID string
}

// ErrNoTarget reports a reference that matched nothing.
var ErrNoTarget = errors.New("no alias matches")

// AmbiguousError reports a reference that matched several aliases. Picking one
// would deliver to the wrong session, so resolution fails and hands back the
// candidates for the sender to choose from.
type AmbiguousError struct {
	Ref        string
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%q matches %d aliases: %s", e.Ref, len(e.Candidates), strings.Join(e.Candidates, ", "))
}

// targetTiers are tried in order. Exact matches come before fuzzy ones so that
// a short alias can never be captured by the substring tier just because it is
// contained in a longer name.
//
// namesASession marks the tiers that address a session rather than a name.
// Since every session now carries an automatic name, a session that is also
// given one by hand answers those tiers with two rows — and two names for one
// session is not an ambiguity, because there is no second session a message
// could go to by mistake. The substring tiers are deliberately not among them:
// they match a pattern, so several hits mean the person has to say which name
// they meant.
//
// livesOnly marks the tier that binds the staleness cutoff as ?2 and so sees
// only the names a live session answers to. It exists because names are never
// reclaimed: a name outlives the session that held it, and every session that
// ever ran leaves one behind. Without this tier the set a person picks from —
// what `pager who` lists — keeps shrinking away from the set a reference is
// resolved against, and abbreviations that were unique when they were learned
// start colliding with the names of sessions that ended long ago.
//
// The wider substring tier still follows it, so a name whose session is gone
// remains addressable; it just no longer competes with one that is not.
var targetTiers = []struct {
	name          string
	query         string
	namesASession bool
	livesOnly     bool
}{
	{"alias", `SELECT alias, COALESCE(session_id, '') FROM aliases WHERE alias = ?1`, false, false},
	{"session", `SELECT alias, COALESCE(session_id, '') FROM aliases WHERE session_id = ?1`, true, false},
	// pm_ref is "KEY/id", so addressing by the bare id is a suffix match.
	// substr with a negative offset counts from the end; LIKE is avoided
	// because the reference itself could contain % or _.
	{"pm_ref", `SELECT a.alias, COALESCE(a.session_id, '')
	              FROM aliases a JOIN sessions s ON s.session_id = a.session_id
	             WHERE s.pm_ref = ?1 OR substr(s.pm_ref, -(length(?1) + 1)) = '/' || ?1`, true, false},
	// instr rather than LIKE, for the same wildcard reason.
	{"live substring", `SELECT a.alias, COALESCE(a.session_id, '')
	                      FROM aliases a
	                     WHERE instr(a.alias, ?1) > 0
	                       AND EXISTS (SELECT 1 FROM sessions s
	                                    WHERE s.session_id = a.session_id
	                                      AND s.heartbeat_at >= ?2)`, false, true},
	{"substring", `SELECT alias, COALESCE(session_id, '') FROM aliases WHERE instr(alias, ?1) > 0`, false, false},
}

// ResolveTarget maps a sender-supplied reference onto an alias.
//
// A tier that matches several aliases fails rather than falling through to the
// next one: several matches means the reference is genuinely ambiguous at that
// level of precision, and continuing would answer a less precise question. That
// is also why the live substring tier does not fall through when it finds
// several — names that live sessions answer to are the ones a person is
// choosing between, and widening the search would only add names to the list
// that nobody is waiting behind.
//
// staleAfter is how long a session may go without a hook event before its names
// stop competing for a short reference. It is passed in rather than read from a
// package constant so that this matches ClaimAlias, OrphanAliases and Roster,
// which all take the same threshold from their caller.
func ResolveTarget(ctx context.Context, st *store.Store, ref string, staleAfter time.Duration) (Target, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Target{}, errors.New("empty target reference")
	}
	cutoff := st.Now() - staleAfter.Milliseconds()

	for _, tier := range targetTiers {
		args := []any{ref}
		if tier.livesOnly {
			args = append(args, cutoff)
		}
		found, err := matchTargets(ctx, st, tier.query, args...)
		if err != nil {
			return Target{}, fmt.Errorf("resolve target by %s: %w", tier.name, err)
		}
		switch {
		case len(found) == 0:
			continue
		case len(found) == 1:
			return found[0], nil
		case tier.namesASession && oneSession(found):
			return primaryTarget(ctx, st, ref, found[0].SessionID)
		default:
			names := make([]string, len(found))
			for i, t := range found {
				names[i] = t.Alias
			}
			return Target{}, &AmbiguousError{Ref: ref, Candidates: names}
		}
	}
	return Target{}, fmt.Errorf("%w: %q", ErrNoTarget, ref)
}

// oneSession reports whether every match belongs to the same, known session.
//
// The empty check is what keeps orphans out of it: an alias whose holder is
// gone carries no session, and two of those are genuinely different inboxes
// even though their session ids compare equal.
func oneSession(found []Target) bool {
	if found[0].SessionID == "" {
		return false
	}
	for _, t := range found[1:] {
		if t.SessionID != found[0].SessionID {
			return false
		}
	}
	return true
}

// primaryTarget resolves a session to the one name it is known by, so that a
// message addressed to the session is addressed to the same inbox a person
// would name.
func primaryTarget(ctx context.Context, st *store.Store, ref, session string) (Target, error) {
	alias, err := PrimaryAlias(ctx, st, session)
	if err != nil {
		return Target{}, err
	}
	if alias == "" {
		return Target{}, fmt.Errorf("%w: %q", ErrNoTarget, ref)
	}
	return Target{Alias: alias, SessionID: session}, nil
}

// matchTargets runs one tier's query. The bindings are variadic because only
// the live tier needs a second one; passing a cutoff the other queries never
// mention would hand SQLite more parameters than the statement declares.
func matchTargets(ctx context.Context, st *store.Store, query string, args ...any) ([]Target, error) {
	rows, err := st.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var found []Target
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.Alias, &t.SessionID); err != nil {
			return nil, err
		}
		found = append(found, t)
	}
	return found, rows.Err()
}

// SetAlias points alias at session, creating it in that session's workspace.
//
// It refuses to move an alias another session holds; that is ClaimAlias's job,
// and only ClaimAlias carries the staleness conditions that make a takeover
// safe. Re-running it for the alias's own session is a no-op that succeeds.
//
// Reports false when the alias belongs to someone else or when session does not
// exist — in both cases nothing was written.
//
// The timestamp is not simply the clock. A session is named automatically when
// it first runs a hook, and the label rule picks a session's most recent name,
// so a name set here has to outrank the one already there. The clock alone
// cannot promise that: it has millisecond resolution, and under a test clock
// that is not advanced the two writes land on the same value, at which point
// the tie-break falls to alphabetical order and the automatic name wins. Taking
// the maximum of the clock and one past the session's newest name makes the
// rule hold regardless of how coarse or frozen the clock is.
//
// The subquery correlates on s.session_id, the row the SELECT is building from;
// excluded.session_id is not available there. The ON CONFLICT predicate is what
// keeps this from moving an alias another session holds, and must survive any
// change to the value above it.
func SetAlias(ctx context.Context, st *store.Store, alias, session string) (bool, error) {
	res, err := st.Exec(ctx, `
		INSERT INTO aliases(alias, session_id, root, tool, updated_at)
		SELECT ?, s.session_id, s.root, s.tool,
		       max(?, COALESCE((SELECT max(a.updated_at) FROM aliases a
		                         WHERE a.session_id = s.session_id), 0) + 1)
		  FROM sessions s WHERE s.session_id = ?
		ON CONFLICT(alias) DO UPDATE SET updated_at = excluded.updated_at
		 WHERE aliases.session_id = excluded.session_id`,
		strings.TrimSpace(alias), st.Now(), session)
	if err != nil {
		return false, fmt.Errorf("set alias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set alias: %w", err)
	}
	return n > 0, nil
}

// ClaimAlias transfers an alias to newSession, reporting whether it succeeded.
//
// There is no automatic handover. Matching root, tool and staleness was once
// enough to inherit an inbox, which meant a session that happened to open the
// same repository today would silently receive mail addressed to yesterday's
// work. Taking over is now something a person asks for.
//
// Every precondition is a condition of this one UPDATE rather than a check
// before it: the alias must be in the claiming session's workspace, that
// session must itself be live, and the current holder must be absent or stale.
// Any of these evaluated separately would leave a window where the holder sends
// a heartbeat between the check and the write, and the claim would succeed
// against a session that is very much alive.
// The timestamp follows the same rule as SetAlias: a name taken over must
// outrank the names the claiming session already holds, so that it becomes the
// one that session is known by. Here the maximum is taken over ?1, the new
// holder, rather than over aliases.session_id — during this UPDATE that column
// still names the incumbent being displaced.
func ClaimAlias(ctx context.Context, st *store.Store, alias, newSession string, staleAfter time.Duration) (bool, error) {
	cutoff := st.Now() - staleAfter.Milliseconds()
	res, err := st.Exec(ctx, `
		UPDATE aliases
		   SET session_id = ?1,
		       lease_generation = lease_generation + 1,
		       updated_at = max(?2, COALESCE((SELECT max(a.updated_at) FROM aliases a
		                                       WHERE a.session_id = ?1), 0) + 1)
		 WHERE alias = ?3
		   -- the claiming session must be live and in this alias's workspace
		   AND EXISTS (SELECT 1 FROM sessions n
		                WHERE n.session_id = ?1
		                  AND n.root = aliases.root AND n.tool = aliases.tool
		                  AND n.heartbeat_at >= ?4)
		   -- the incumbent must be absent or stale
		   AND (session_id IS NULL
		        OR NOT EXISTS (SELECT 1 FROM sessions o
		                        WHERE o.session_id = aliases.session_id
		                          AND o.heartbeat_at >= ?4))`,
		newSession, st.Now(), strings.TrimSpace(alias), cutoff)
	if err != nil {
		return false, fmt.Errorf("claim alias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim alias: %w", err)
	}
	return n > 0, nil
}

// Orphan is an alias whose holder is gone while mail is still waiting for it.
type Orphan struct {
	Alias   string
	Pending int
}

// SessionWorkspace returns the root and tool stored against a session, or two
// empty strings when no row exists yet.
//
// Callers pass the result to OrphanAliases, which matches an alias on exactly
// this pair. Reading the stored row rather than whatever the caller holds is the
// same rule workspaceKnown records for the naming path: a hook whose host
// detection just failed carries an empty tool, but RecordSession leaves an
// already-known value in place, so the session's workspace is still known even
// though this particular invocation could not see it. Judging from the caller's
// own record instead makes the orphan hint disappear for exactly as long as
// detection keeps missing.
//
// A missing row is not an error for the same reason it is not one there: a
// session that has never been recorded simply has no workspace, and the caller's
// empty-string check already covers it.
func SessionWorkspace(ctx context.Context, st *store.Store, session string) (root, tool string, err error) {
	err = st.DB().QueryRowContext(ctx,
		"SELECT root, tool FROM sessions WHERE session_id = ?", session).Scan(&root, &tool)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read session workspace: %w", err)
	}
	return root, tool, nil
}

// OrphanAliases lists the orphans in one workspace.
//
// Undelivered mail is part of the definition on purpose. An alias whose session
// simply ended, with nothing waiting, is not a problem to solve and suggesting
// a takeover for it would be noise.
func OrphanAliases(ctx context.Context, st *store.Store, root, tool string, staleAfter time.Duration) ([]Orphan, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT a.alias, count(m.id)
		  FROM aliases a
		  JOIN messages m ON m.alias = a.alias AND m.delivered_at IS NULL
		 WHERE a.root = ? AND a.tool = ?
		   AND (a.session_id IS NULL
		        OR NOT EXISTS (SELECT 1 FROM sessions o
		                        WHERE o.session_id = a.session_id
		                          AND o.heartbeat_at >= ?))
		 GROUP BY a.alias
		 ORDER BY a.alias`,
		root, tool, st.Now()-staleAfter.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("list orphan aliases: %w", err)
	}
	defer rows.Close()

	var orphans []Orphan
	for rows.Next() {
		var o Orphan
		if err := rows.Scan(&o.Alias, &o.Pending); err != nil {
			return nil, fmt.Errorf("list orphan aliases: %w", err)
		}
		orphans = append(orphans, o)
	}
	return orphans, rows.Err()
}

// TakeClaimHint reserves the single orphan hint a session is allowed to show,
// reporting whether this caller got it.
//
// Reserving it with a conditional update rather than reading the column and
// then writing it is what keeps concurrent hooks in one session from each
// deciding they are the first.
func TakeClaimHint(ctx context.Context, st *store.Store, session string) (bool, error) {
	res, err := st.Exec(ctx,
		"UPDATE sessions SET claim_hint_at = ? WHERE session_id = ? AND claim_hint_at IS NULL",
		st.Now(), session)
	if err != nil {
		return false, fmt.Errorf("take claim hint: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("take claim hint: %w", err)
	}
	return n > 0, nil
}

// LeaseGeneration returns an alias's takeover counter, or sql.ErrNoRows when
// the alias does not exist.
func LeaseGeneration(ctx context.Context, st *store.Store, alias string) (int, error) {
	var gen int
	err := st.DB().QueryRowContext(ctx,
		"SELECT lease_generation FROM aliases WHERE alias = ?", alias).Scan(&gen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read lease generation: %w", err)
	}
	return gen, err
}
