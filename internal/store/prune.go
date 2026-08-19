package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Retention defaults. The plan records these as estimates — no production
// signal informed them.
const (
	// DefaultRetention is how long a delivered message survives before prune
	// deletes it. It is far longer than the injection window (72h), so the
	// cause of an active chain is never a prune candidate in practice.
	DefaultRetention = 30 * 24 * time.Hour

	// pruneGate is the minimum interval between opportunistic prunes.
	pruneGate = 24 * time.Hour

	// pruneLease bounds how long a runner may hold the gate before another
	// process may assume it was stranded mid-prune and take over.
	pruneLease = 10 * time.Minute

	// pruneBatch caps one pass so a hook never blocks on a large delete.
	pruneBatch = 1000
)

// PruneResult reports what a prune call did. Deleted is meaningful only when
// Ran is true; an opportunistic call that lost the gate reports Ran false.
type PruneResult struct {
	Ran     bool
	Deleted int64
}

// deletable selects retention-expired messages that nothing still depends on.
//
// The two NOT IN clauses are not an optimisation — foreign_keys is ON, so
// deleting a row that sessions.last_inbound_id or messages.cause_id still
// points at would fail the whole statement and make prune permanently
// unable to make progress. Excluding them is what lets prune stay quiet.
const deletable = `
  SELECT id FROM messages
   WHERE delivered_at IS NOT NULL
     AND created_at < ?
     AND id NOT IN (SELECT last_inbound_id FROM sessions WHERE last_inbound_id IS NOT NULL)
     AND id NOT IN (SELECT cause_id FROM messages WHERE cause_id IS NOT NULL)`

// PruneNow deletes expired messages regardless of the interval — the
// `pager prune` path. With dryRun it only counts, changing nothing.
//
// It still takes the lease. Ignoring the interval is the point of asking for a
// prune; ignoring the exclusion as well would let this call and a hook's
// opportunistic prune append to the archive at the same offset, and the loser's
// records would be gone from the file while still being deleted from the
// database. A call that cannot take the lease reports Ran false and does
// nothing, which is what `pager prune` renders.
//
// dryRun deliberately takes no lease: it writes nothing, so a prune under way is
// no reason for a count to fail.
func (s *Store) PruneNow(ctx context.Context, retention time.Duration, dryRun bool) (PruneResult, error) {
	cutoff := s.Now() - retention.Milliseconds()
	if dryRun {
		var n int64
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM ("+deletable+")", cutoff).Scan(&n); err != nil {
			return PruneResult{}, fmt.Errorf("count prunable: %w", err)
		}
		return PruneResult{Ran: true, Deleted: n}, nil
	}

	token, err := s.acquirePrune(ctx, pruneOnDemand)
	if err != nil || token == "" {
		return PruneResult{}, err
	}
	// Released on every exit, unlike the scheduled path which keeps the lease on
	// failure so the interval stays unadvanced. A manual prune's failure is meant
	// to be read and retried at once — README points at `pager prune` erroring as
	// the way an archive problem shows up — so holding the lease afterwards would
	// block the retry that fixes it.
	defer func() { _, _ = s.releasePruneLease(ctx, token) }()

	n, err := s.deleteExpired(ctx, cutoff)
	return PruneResult{Ran: true, Deleted: n}, err
}

// PruneOpportunistic performs at most one prune per gate interval across all
// processes. Hooks call it on every event; only the process that wins the gate
// does any work.
//
// The completion time is recorded only on success. A failed prune therefore
// leaves the gate interval unadvanced and the next hook retries, rather than
// the failure silently buying another 24h of growth.
func (s *Store) PruneOpportunistic(ctx context.Context, retention time.Duration) (PruneResult, error) {
	token, err := s.acquirePruneGate(ctx)
	if err != nil || token == "" {
		return PruneResult{}, err
	}
	n, err := s.deleteExpired(ctx, s.Now()-retention.Milliseconds())
	if err != nil {
		return PruneResult{Ran: true}, err
	}
	if _, err := s.releasePruneGate(ctx, token); err != nil {
		return PruneResult{Ran: true, Deleted: n}, err
	}
	return PruneResult{Ran: true, Deleted: n}, nil
}

// pruneMode says which half of the gate a caller needs.
//
// The two halves are separable, and the manual path needs only one of them. A
// prune asked for on demand must bypass the once-a-day interval — that is what
// asking for it means — but it must not bypass the exclusion, because the
// archive append is a file write outside any transaction: two appends racing
// compute the same end offset and overwrite each other's records, leaving a file
// that still parses and is simply missing them.
type pruneMode int

const (
	pruneScheduled pruneMode = iota // honours the interval and the exclusion
	pruneOnDemand                   // exclusion only
)

// acquirePruneGate tries to take the whole gate — the hook path.
func (s *Store) acquirePruneGate(ctx context.Context) (string, error) {
	return s.acquirePrune(ctx, pruneScheduled)
}

// acquirePrune tries to take the gate, returning a non-empty token when this
// process won and must perform the prune.
//
// Every condition lives inside the UPDATE. Checking them first and updating
// after would let every hook that observed "24h elapsed" believe it had won.
//
// The exclusion clause keys on lease_token, which both release paths clear, so
// it means "nobody is pruning right now" — and prune_started_at only decides
// whether a holder has been there long enough to be presumed stranded. Reading
// exclusion off prune_started_at alone would be wrong, because releasing does
// not clear it: a prune that finished a minute ago would still look like one in
// flight. The scheduled path hides that, since its 24h prune_done_at clause
// already blocks anything that recent; pruneOnDemand has no such cover and would
// refuse for the whole lease interval after any prune completed.
func (s *Store) acquirePrune(ctx context.Context, mode pruneMode) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("prune token: %w", err)
	}
	token := hex.EncodeToString(buf[:])

	now := s.Now()
	query := `
		UPDATE meta
		   SET lease_token = ?, prune_started_at = ?
		 WHERE key = 'prune'
		   AND (lease_token IS NULL OR prune_started_at IS NULL OR prune_started_at < ?)`
	args := []any{token, now, now - pruneLease.Milliseconds()}
	if mode == pruneScheduled {
		query += " AND (prune_done_at IS NULL OR prune_done_at < ?)"
		args = append(args, now-pruneGate.Milliseconds())
	}

	res, err := s.Exec(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("acquire prune gate: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("acquire prune gate: %w", err)
	}
	if n == 0 {
		return "", nil // another process holds the gate, or it ran recently
	}
	return token, nil
}

// releasePruneGate records completion, and reports whether it took effect.
//
// The token condition matters when a runner was declared stranded and a second
// runner took over: the first one finishing late must not stamp a completion
// time over the successor's in-progress state.
func (s *Store) releasePruneGate(ctx context.Context, token string) (bool, error) {
	res, err := s.Exec(ctx,
		"UPDATE meta SET prune_done_at = ?, lease_token = NULL WHERE key = 'prune' AND lease_token = ?",
		s.Now(), token)
	if err != nil {
		return false, fmt.Errorf("release prune gate: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("release prune gate: %w", err)
	}
	return n > 0, nil
}

// releasePruneLease drops the lease without recording completion.
//
// A manual prune must not consume the day's scheduled slot: suppressing the
// automatic prune for the next 24h would be a change to the retention policy
// rather than the serialisation this lease exists for. The token condition
// carries the same stranded-runner meaning it does in releasePruneGate.
func (s *Store) releasePruneLease(ctx context.Context, token string) (bool, error) {
	res, err := s.Exec(ctx,
		"UPDATE meta SET lease_token = NULL WHERE key = 'prune' AND lease_token = ?", token)
	if err != nil {
		return false, fmt.Errorf("release prune lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("release prune lease: %w", err)
	}
	return n > 0, nil
}

// deleteExpired archives at most pruneBatch expired, unreferenced messages and
// then removes them.
//
// The archive write happens outside any transaction, on purpose. Holding the
// SQLite write lock across an fsync would block every concurrent hook for up to
// the entire 5s budget one gets — busy_timeout is 5s too — and a hook deadline
// expiring mid-write could not roll a file back anyway.
//
// What makes that safe is the shape of the DELETE rather than a transaction: it
// names the ids that were just archived, so nothing outside the archived set
// can be removed, and it re-applies the deletable predicate, so a row that
// gained a reference since the read is skipped instead of failing the whole
// statement on its foreign key. A row archived but not deleted is simply
// archived again next pass — see the at-least-once contract in the plan.
//
// When archiving is switched off the guarantee is explicitly given up and the
// delete proceeds exactly as it did before.
func (s *Store) deleteExpired(ctx context.Context, cutoff int64) (int64, error) {
	recs, err := s.selectDeletable(ctx, cutoff, pruneBatch)
	if err != nil || len(recs) == 0 {
		return 0, err
	}
	if archiveEnabled() {
		if err := appendArchive(archivePath(s.path), recs); err != nil {
			return 0, err
		}
	}
	return s.deleteArchived(ctx, recs, cutoff)
}

// selectDeletable reads the next batch of prune candidates in full, because
// they have to be archived before they can be deleted.
func (s *Store) selectDeletable(ctx context.Context, cutoff int64, limit int) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+recordColumns+" FROM messages WHERE id IN ("+deletable+" LIMIT ?)", cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("select prunable: %w", err)
	}
	defer rows.Close()

	var out []Record
	if err := eachRecord(rows, func(r Record) error {
		out = append(out, r)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("select prunable: %w", err)
	}
	return out, nil
}

// deleteArchived removes exactly those of recs that are still deletable.
//
// One statement, so it is its own transaction and Exec's SQLITE_BUSY retry
// applies unchanged — there is no file write inside it to repeat.
func (s *Store) deleteArchived(ctx context.Context, recs []Record, cutoff int64) (int64, error) {
	res, err := s.Exec(ctx,
		"DELETE FROM messages WHERE id IN ("+idList(recs)+") AND id IN ("+deletable+")", cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune messages: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune messages: %w", err)
	}
	return n, nil
}

// idList renders record ids as a SQL list.
//
// These integers came out of the database moments ago, so there is nothing to
// inject; writing them literally also keeps a full batch clear of the bound
// parameter ceiling older SQLite builds impose.
func idList(recs []Record) string {
	var sb strings.Builder
	for i, r := range recs {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatInt(r.ID, 10))
	}
	return sb.String()
}
