package store

import (
	"context"
	"fmt"
)

// ID returns this database's own store id: the random value a peer enrols so
// that a relay landing on a different database is refused rather than
// delivered. It is created by the migration that introduced relaying, so every
// opened store has exactly one.
func (s *Store) ID(ctx context.Context) (string, error) {
	var id string
	if err := s.db.QueryRowContext(ctx,
		"SELECT id FROM store_identity WHERE singleton = 1").Scan(&id); err != nil {
		return "", fmt.Errorf("read store id: %w", err)
	}
	return id, nil
}
