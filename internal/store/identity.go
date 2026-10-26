package store

import (
	"context"
	"database/sql"
	"fmt"
)

const idQuery = "SELECT id FROM store_identity WHERE singleton = 1"

// ID returns this database's own store id: the random value a peer enrols so
// that a relay landing on a different database is refused rather than
// delivered. It is created by the migration that introduced relaying, so every
// opened store has exactly one.
func (s *Store) ID(ctx context.Context) (string, error) {
	var id string
	if err := s.db.QueryRowContext(ctx, idQuery).Scan(&id); err != nil {
		return "", fmt.Errorf("read store id: %w", err)
	}
	return id, nil
}

// IDOn reads the store id on c, so a caller inside WriteTx sees the id its
// own transaction would — not one another connection is about to replace.
func IDOn(ctx context.Context, c *sql.Conn) (string, error) {
	var id string
	if err := c.QueryRowContext(ctx, idQuery).Scan(&id); err != nil {
		return "", fmt.Errorf("read store id: %w", err)
	}
	return id, nil
}
