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

	"github.com/iMUngHee/pager/internal/presence"
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

// ErrAliasHasAt refuses a new alias containing '@', which addresses a peer.
var ErrAliasHasAt = errors.New("an alias cannot contain '@' — name@peer addresses a session in another store")

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
// livesOnly marks the tier filtered through the same presence policy as the
// default roster. Names are never reclaimed: a name outlives the session that
// held it, and every session that
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
	{"alias", exactAliasQuery, false, false},
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
	                       AND EXISTS (SELECT 1 FROM sessions s WHERE s.session_id = a.session_id)`, false, true},
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
// staleAfter controls recent unknown sessions; positive runtime evidence can
// keep an older session competing for a short reference. It is passed in rather
// than read from a package constant so that this matches ClaimAlias,
// OrphanAliases and Roster, which take the same threshold from their caller.
func ResolveTarget(ctx context.Context, st *store.Store, ref string, staleAfter time.Duration, probes ...presence.Probe) (Target, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Target{}, errors.New("empty target reference")
	}
	var probe presence.Probe
	if len(probes) > 0 {
		probe = probes[0]
	}

	for _, tier := range targetTiers {
		args := []any{ref}
		found, err := matchTargets(ctx, st, tier.query, args...)
		if err != nil {
			return Target{}, fmt.Errorf("resolve target by %s: %w", tier.name, err)
		}
		if tier.livesOnly && len(found) > 0 {
			view, err := Roster(ctx, st, staleAfter, RosterOptions{Probe: probe})
			if err != nil {
				return Target{}, err
			}
			visible := make(map[string]bool, len(view.Entries))
			for _, e := range view.Entries {
				visible[e.SessionID] = true
			}
			eligible := found[:0]
			for _, t := range found {
				if visible[t.SessionID] {
					eligible = append(eligible, t)
				}
			}
			found = eligible
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

// exactAliasQuery is the first tier: a reference that is an alias's own name.
const exactAliasQuery = `SELECT alias, COALESCE(session_id, '') FROM aliases WHERE alias = ?1`

// ResolveExact resolves a reference by its exact alias spelling and nothing
// else. It is what a legacy alias containing '@' gets: through the wider tiers
// such a name could match a different one.
func ResolveExact(ctx context.Context, st *store.Store, alias string) (Target, error) {
	found, err := matchTargets(ctx, st, exactAliasQuery, alias)
	if err != nil {
		return Target{}, fmt.Errorf("resolve target by alias: %w", err)
	}
	if len(found) == 0 {
		return Target{}, fmt.Errorf("%w: %q", ErrNoTarget, alias)
	}
	return found[0], nil
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

// matchTargets runs one address tier's query.
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
// It refuses to move an alias another session holds; ClaimAlias checks runtime
// evidence and a recorded snapshot before a takeover. Re-running it for the
// alias's own session is a no-op that succeeds.
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
//
// A name containing '@' is refused: name@peer is how a sender addresses a
// session in another store, and a local alias spelled the same way would make
// that address mean two things. Aliases created before this rule keep working —
// ClaimAlias still takes them over — but no new one can be made.
func SetAlias(ctx context.Context, st *store.Store, alias, session string) (bool, error) {
	alias = strings.TrimSpace(alias)
	if strings.Contains(alias, "@") {
		return false, fmt.Errorf("%w: %q", ErrAliasHasAt, alias)
	}
	res, err := st.Exec(ctx, `
		INSERT INTO aliases(alias, session_id, root, tool, updated_at)
		SELECT ?, s.session_id, s.root, s.tool,
		       max(?, COALESCE((SELECT max(a.updated_at) FROM aliases a
		                         WHERE a.session_id = s.session_id), 0) + 1)
		  FROM sessions s WHERE s.session_id = ?
		ON CONFLICT(alias) DO UPDATE SET updated_at = excluded.updated_at
		 WHERE aliases.session_id = excluded.session_id`,
		alias, st.Now(), session)
	if err != nil {
		return false, fmt.Errorf("set alias: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set alias: %w", err)
	}
	return n > 0, nil
}

// ClaimOptions permits a deliberate takeover of a stale unknown holder. A
// positive runtime observation or a recent hook cannot be overridden.
type ClaimOptions struct {
	Probe        presence.Probe
	AllowUnknown bool
}

// ClaimAlias probes the incumbent, then compares its recorded snapshot in the
// UPDATE. Runtime observations are not atomic with SQLite, but a hook refresh,
// host rebind or lease transfer recorded during the probe refuses the claim.
func ClaimAlias(ctx context.Context, st *store.Store, alias, newSession string, staleAfter time.Duration, options ...ClaimOptions) (bool, error) {
	var opts ClaimOptions
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.Probe == nil {
		opts.Probe = presence.NewProbe()
	}
	var holder, id, tool, root, hostClient sql.NullString
	var heartbeat, pid, start sql.NullInt64
	var generation int64
	err := st.DB().QueryRowContext(ctx, `
  SELECT a.session_id,a.lease_generation,s.session_id,s.tool,s.root,
         s.heartbeat_at,s.host_client,s.host_pid,s.host_start
    FROM aliases a LEFT JOIN sessions s ON s.session_id=a.session_id WHERE a.alias=?`, strings.TrimSpace(alias)).Scan(
		&holder, &generation, &id, &tool, &root, &heartbeat, &hostClient, &pid, &start)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read claim holder: %w", err)
	}
	if id.Valid {
		session := presence.Session{ID: id.String, Tool: tool.String, Heartbeat: heartbeat.Int64, HostPID: int(pid.Int64), HostStart: start.Int64}
		state := presence.Classify(session, opts.Probe(ctx, session), st.Now(), staleAfter)
		if !state.Gone && !(opts.AllowUnknown && !state.Recent && !state.Available) {
			return false, nil
		}
	}
	res, err := st.Exec(ctx, `
  UPDATE aliases
     SET session_id=?1,lease_generation=lease_generation+1,
         updated_at=max(?2,COALESCE((SELECT max(a.updated_at) FROM aliases a WHERE a.session_id=?1),0)+1)
   WHERE alias=?3 AND session_id IS ?4 AND lease_generation=?5
     AND EXISTS (SELECT 1 FROM sessions n WHERE n.session_id=?1
                 AND n.root=aliases.root AND n.tool=aliases.tool AND n.heartbeat_at>=?6)
     AND ((?7 IS NULL AND NOT EXISTS (SELECT 1 FROM sessions o WHERE o.session_id=aliases.session_id))
          OR EXISTS (SELECT 1 FROM sessions o WHERE o.session_id=?7 AND o.session_id=aliases.session_id
                      AND o.heartbeat_at IS ?8 AND o.host_client IS ?9
                      AND o.host_pid IS ?10 AND o.host_start IS ?11
                      AND o.tool IS ?12 AND o.root IS ?13))`,
		newSession, st.Now(), strings.TrimSpace(alias), holder, generation, st.Now()-staleAfter.Milliseconds(),
		id, heartbeat, hostClient, pid, start, tool, root)
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
func OrphanAliases(ctx context.Context, st *store.Store, root, tool string, staleAfter time.Duration, probes ...presence.Probe) ([]Orphan, error) {
	probe := presence.NewProbe()
	if len(probes) > 0 && probes[0] != nil {
		probe = probes[0]
	}
	rows, err := st.DB().QueryContext(ctx, `
  SELECT a.alias,count(m.id),COALESCE(s.session_id,''),COALESCE(s.tool,''),
         COALESCE(s.heartbeat_at,0),COALESCE(s.host_pid,0),COALESCE(s.host_start,0)
    FROM aliases a
    JOIN messages m ON m.alias=a.alias AND m.delivered_at IS NULL AND m.dst_peer IS NULL
    LEFT JOIN sessions s ON s.session_id=a.session_id
   WHERE a.root=? AND a.tool=? GROUP BY a.alias ORDER BY a.alias`, root, tool)
	if err != nil {
		return nil, fmt.Errorf("list orphan aliases: %w", err)
	}
	type candidate struct {
		orphan  Orphan
		session presence.Session
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.orphan.Alias, &c.orphan.Pending, &c.session.ID, &c.session.Tool, &c.session.Heartbeat, &c.session.HostPID, &c.session.HostStart); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var orphans []Orphan
	for _, c := range candidates {
		if c.session.ID == "" || presence.Classify(c.session, probe(ctx, c.session), st.Now(), staleAfter).Gone {
			orphans = append(orphans, c.orphan)
		}
	}
	return orphans, nil
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
