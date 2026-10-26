package deliver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/iMUngHee/pager/internal/store"
)

// A peer is another store this one relays mail with. pager itself never
// reaches a peer: something outside it — the transport — carries `pager relay
// out` from one store into `pager relay in` on the other. What the core keeps
// is only the name a sender writes after '@', the id of the store that name
// was enrolled against, and the allocator of the current outbound link.

var (
	// ErrUnknownPeer reports a peer name that is not registered here.
	ErrUnknownPeer = errors.New("unknown peer")
	// ErrPeerHasPendingMail refuses to forget a peer that still has mail
	// waiting to leave for it: removing it would strand those rows with no way
	// to export or acknowledge them.
	ErrPeerHasPendingMail = errors.New("peer still has mail waiting to leave")
)

// maxPeerName bounds a peer name. The transport puts it on a command line, so
// it is kept short and to a character set no shell treats specially.
const maxPeerName = 32

// Peer is one registered store.
type Peer struct {
	Name    string
	StoreID string
	LinkID  string
	OutSeq  int64
	AddedAt int64
	// Pending counts outbound rows to this peer that have no final result yet.
	Pending int
}

// NormalizePeerName lower-cases a peer name and checks it is 1–32 characters
// of [a-z0-9-].
func NormalizePeerName(name string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" || len(n) > maxPeerName {
		return "", fmt.Errorf("peer name %q must be 1-%d characters", name, maxPeerName)
	}
	for _, r := range n {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return "", fmt.Errorf("peer name %q may only use a-z, 0-9 and '-'", name)
		}
	}
	return n, nil
}

// validStoreID reports whether id has the shape of a store id: 32 lowercase
// hex digits, as the relay migration generates them.
func validStoreID(id string) bool {
	if len(id) != 32 {
		return false
	}
	return strings.Trim(id, "0123456789abcdef") == ""
}

// newLinkID mints the random id of a link. A link starts whenever a pairing is
// made or remade, so its sequence can restart at 1 without colliding with
// anything the other side remembers about an earlier one.
func newLinkID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint link id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// AddPeer registers a store under name.
//
// storeID is the other store's own id (`pager relay id` there). Enrolling it
// explicitly, rather than trusting whichever store first answers, is what
// makes a transport that lands on the wrong database fail instead of
// delivering there.
//
// A name that an existing alias already ends with as "@name" is refused: an
// address resolves an exact local alias before it looks for a peer, so such an
// alias would silently shadow the peer.
func AddPeer(ctx context.Context, st *store.Store, name, storeID string) (Peer, error) {
	name, err := NormalizePeerName(name)
	if err != nil {
		return Peer{}, err
	}
	storeID = strings.ToLower(strings.TrimSpace(storeID))
	if !validStoreID(storeID) {
		return Peer{}, fmt.Errorf("store id %q is not 32 hex digits — copy it from `pager relay id` on that store", storeID)
	}
	own, err := st.ID(ctx)
	if err != nil {
		return Peer{}, err
	}
	if storeID == own {
		return Peer{}, errors.New("that is this store's own id; a peer is another store")
	}
	link, err := newLinkID()
	if err != nil {
		return Peer{}, err
	}

	p := Peer{Name: name, StoreID: storeID, LinkID: link, AddedAt: st.Now()}
	err = st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		var exists int
		if err := c.QueryRowContext(ctx,
			"SELECT count(*) FROM peers WHERE name = ?", name).Scan(&exists); err != nil {
			return fmt.Errorf("read peers: %w", err)
		}
		if exists > 0 {
			return fmt.Errorf("peer %q already exists", name)
		}

		suffix := "@" + name
		rows, err := c.QueryContext(ctx,
			"SELECT alias FROM aliases WHERE lower(substr(alias, -?)) = ? ORDER BY alias",
			len(suffix), suffix)
		if err != nil {
			return fmt.Errorf("read aliases: %w", err)
		}
		var shadowing []string
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				rows.Close()
				return fmt.Errorf("read aliases: %w", err)
			}
			shadowing = append(shadowing, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read aliases: %w", err)
		}
		if len(shadowing) > 0 {
			return fmt.Errorf("peer %q would be shadowed by existing aliases ending in %q: %s",
				name, suffix, strings.Join(shadowing, ", "))
		}

		if _, err := c.ExecContext(ctx,
			"INSERT INTO peers(name, store_id, link_id, out_seq, added_at) VALUES (?, ?, ?, 0, ?)",
			p.Name, p.StoreID, p.LinkID, p.AddedAt); err != nil {
			return fmt.Errorf("insert peer: %w", err)
		}
		return nil
	})
	if err != nil {
		return Peer{}, err
	}
	return p, nil
}

// ListPeers returns every registered peer, by name.
func ListPeers(ctx context.Context, st *store.Store) ([]Peer, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT p.name, p.store_id, p.link_id, p.out_seq, p.added_at,
		       (SELECT count(*) FROM messages m WHERE m.dst_peer = p.name AND m.relayed_at IS NULL)
		  FROM peers p
		 ORDER BY p.name`)
	if err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	defer rows.Close()

	var out []Peer
	for rows.Next() {
		var p Peer
		if err := rows.Scan(&p.Name, &p.StoreID, &p.LinkID, &p.OutSeq, &p.AddedAt, &p.Pending); err != nil {
			return nil, fmt.Errorf("list peers: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RemovePeer forgets a peer that has nothing waiting to leave for it.
//
// The pending check and the delete share one write transaction, so a send
// that commits first makes this refuse, and one that commits after finds no
// peer row and fails itself. Inbound link state is kept: if the same store is
// enrolled again, its links continue from where they were instead of being
// replayed or refused.
func RemovePeer(ctx context.Context, st *store.Store, name string) error {
	name, err := NormalizePeerName(name)
	if err != nil {
		return err
	}
	return st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		var exists, pending int
		if err := c.QueryRowContext(ctx, `
			SELECT (SELECT count(*) FROM peers WHERE name = ?1),
			       (SELECT count(*) FROM messages WHERE dst_peer = ?1 AND relayed_at IS NULL)`,
			name).Scan(&exists, &pending); err != nil {
			return fmt.Errorf("read peer %q: %w", name, err)
		}
		if exists == 0 {
			return fmt.Errorf("%w %q", ErrUnknownPeer, name)
		}
		if pending > 0 {
			return fmt.Errorf("%w: %d message(s) to %q have no final result; "+
				"relay them first, or remove with --force to cancel them", ErrPeerHasPendingMail, pending, name)
		}
		if _, err := c.ExecContext(ctx, "DELETE FROM peers WHERE name = ?", name); err != nil {
			return fmt.Errorf("remove peer %q: %w", name, err)
		}
		return nil
	})
}
