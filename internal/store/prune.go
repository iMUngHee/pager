package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
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

// PruneNow deletes expired messages unconditionally — the `pager prune` path.
// With dryRun it only counts, changing nothing.
func (s *Store) PruneNow(ctx context.Context, retention time.Duration, dryRun bool) (PruneResult, error) {
	cutoff := s.Now() - retention.Milliseconds()
	if dryRun {
		var n int64
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM ("+deletable+")", cutoff).Scan(&n); err != nil {
			return PruneResult{}, fmt.Errorf("count prunable: %w", err)
		}
		return PruneResult{Ran: true, Deleted: n}, nil
	}
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

// acquirePruneGate tries to take the prune gate, returning a non-empty token
// when this process won and must perform the prune.
//
// Both conditions live inside the UPDATE. Checking them first and updating
// after would let every hook that observed "24h elapsed" believe it had won.
// The started_at clause is what distinguishes "nobody is pruning" from "a
// prune is under way" — without it the gate only blocks runners that arrive
// after the first one has already recorded completion, which is no gate at all.
func (s *Store) acquirePruneGate(ctx context.Context) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("prune token: %w", err)
	}
	token := hex.EncodeToString(buf[:])

	now := s.Now()
	res, err := s.Exec(ctx, `
		UPDATE meta
		   SET lease_token = ?, prune_started_at = ?
		 WHERE key = 'prune'
		   AND (prune_done_at    IS NULL OR prune_done_at    < ?)
		   AND (prune_started_at IS NULL OR prune_started_at < ?)`,
		token, now, now-pruneGate.Milliseconds(), now-pruneLease.Milliseconds())
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

// deleteExpired removes at most pruneBatch expired, unreferenced messages.
func (s *Store) deleteExpired(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.Exec(ctx,
		"DELETE FROM messages WHERE id IN ("+deletable+" LIMIT ?)", cutoff, pruneBatch)
	if err != nil {
		return 0, fmt.Errorf("prune messages: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune messages: %w", err)
	}
	return n, nil
}
