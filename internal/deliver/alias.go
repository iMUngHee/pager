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

	"github.com/unghee/pager/internal/store"
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
var targetTiers = []struct {
	name  string
	query string
}{
	{"alias", `SELECT alias, COALESCE(session_id, '') FROM aliases WHERE alias = ?1`},
	{"session", `SELECT alias, COALESCE(session_id, '') FROM aliases WHERE session_id = ?1`},
	// pm_ref is "KEY/id", so addressing by the bare id is a suffix match.
	// substr with a negative offset counts from the end; LIKE is avoided
	// because the reference itself could contain % or _.
	{"pm_ref", `SELECT a.alias, COALESCE(a.session_id, '')
	              FROM aliases a JOIN sessions s ON s.session_id = a.session_id
	             WHERE s.pm_ref = ?1 OR substr(s.pm_ref, -(length(?1) + 1)) = '/' || ?1`},
	// instr rather than LIKE, for the same wildcard reason.
	{"substring", `SELECT alias, COALESCE(session_id, '') FROM aliases WHERE instr(alias, ?1) > 0`},
}

// ResolveTarget maps a sender-supplied reference onto an alias.
//
// A tier that matches several aliases fails rather than falling through to the
// next one: several matches means the reference is genuinely ambiguous at that
// level of precision, and continuing would answer a less precise question.
func ResolveTarget(ctx context.Context, st *store.Store, ref string) (Target, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Target{}, errors.New("empty target reference")
	}

	for _, tier := range targetTiers {
		found, err := matchTargets(ctx, st, tier.query, ref)
		if err != nil {
			return Target{}, fmt.Errorf("resolve target by %s: %w", tier.name, err)
		}
		switch len(found) {
		case 0:
			continue
		case 1:
			return found[0], nil
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

func matchTargets(ctx context.Context, st *store.Store, query, ref string) ([]Target, error) {
	rows, err := st.DB().QueryContext(ctx, query, ref)
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
func SetAlias(ctx context.Context, st *store.Store, alias, session string) (bool, error) {
	res, err := st.Exec(ctx, `
		INSERT INTO aliases(alias, session_id, root, tool, updated_at)
		SELECT ?, s.session_id, s.root, s.tool, ?
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
func ClaimAlias(ctx context.Context, st *store.Store, alias, newSession string, staleAfter time.Duration) (bool, error) {
	cutoff := st.Now() - staleAfter.Milliseconds()
	res, err := st.Exec(ctx, `
		UPDATE aliases
		   SET session_id = ?, lease_generation = lease_generation + 1, updated_at = ?
		 WHERE alias = ?
		   -- the claiming session must be live and in this alias's workspace
		   AND EXISTS (SELECT 1 FROM sessions n
		                WHERE n.session_id = ?
		                  AND n.root = aliases.root AND n.tool = aliases.tool
		                  AND n.heartbeat_at >= ?)
		   -- the incumbent must be absent or stale
		   AND (session_id IS NULL
		        OR NOT EXISTS (SELECT 1 FROM sessions o
		                        WHERE o.session_id = aliases.session_id
		                          AND o.heartbeat_at >= ?))`,
		newSession, st.Now(), strings.TrimSpace(alias), newSession, cutoff, cutoff)
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
