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
	for i, r := range n {
		alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !alnum && (r != '-' || i == 0) {
			// A leading '-' would read as a flag wherever a transport puts
			// the name on a command line.
			return "", fmt.Errorf("peer name %q may only use a-z, 0-9 and '-', and must start with a letter or digit", name)
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
	link, err := newLinkID()
	if err != nil {
		return Peer{}, err
	}

	p := Peer{Name: name, StoreID: storeID, LinkID: link, AddedAt: st.Now()}
	err = st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		own, err := store.IDOn(ctx, c)
		if err != nil {
			return err
		}
		if storeID == own {
			return errors.New("that is this store's own id; a peer is another store")
		}

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

// RebindPeer points an existing peer at a different store id, or re-pairs it
// with the same one, and starts a new link.
//
// Mail still waiting for the peer moves to the new link, renumbered from 1 in
// the order it was sent, in the same transaction that mints the link — so the
// other store sees a fresh link with no gap, and a result still in flight for
// the old link matches no row and changes nothing. What a transport already
// carried to the old store cannot be recalled: if that store is still alive,
// it may receive those messages too.
func RebindPeer(ctx context.Context, st *store.Store, name, storeID string) (Peer, error) {
	name, err := NormalizePeerName(name)
	if err != nil {
		return Peer{}, err
	}
	storeID = strings.ToLower(strings.TrimSpace(storeID))
	if !validStoreID(storeID) {
		return Peer{}, fmt.Errorf("store id %q is not 32 hex digits — copy it from `pager relay id` on that store", storeID)
	}
	link, err := newLinkID()
	if err != nil {
		return Peer{}, err
	}
	var p Peer
	err = st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		own, err := store.IDOn(ctx, c)
		if err != nil {
			return err
		}
		if storeID == own {
			return errors.New("that is this store's own id; a peer is another store")
		}
		if err := c.QueryRowContext(ctx,
			"SELECT added_at FROM peers WHERE name = ?", name).Scan(&p.AddedAt); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w %q", ErrUnknownPeer, name)
		} else if err != nil {
			return fmt.Errorf("read peer %q: %w", name, err)
		}
		n, err := moveToLink(ctx, c, name, link)
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx,
			"UPDATE peers SET store_id = ?, link_id = ?, out_seq = ? WHERE name = ?",
			storeID, link, n, name); err != nil {
			return fmt.Errorf("rebind peer %q: %w", name, err)
		}
		p = Peer{Name: name, StoreID: storeID, LinkID: link, OutSeq: n, AddedAt: p.AddedAt, Pending: int(n)}
		return nil
	})
	if err != nil {
		return Peer{}, err
	}
	return p, nil
}

// RotateStoreID gives this store a new id, and every peer a new link.
//
// It is what a store restored from a backup needs: the restore rolls its
// sequence numbers and message ids back while keeping the id, so a peer would
// answer genuinely new mail as already seen. With a new id every link is new on
// both sides, and each peer must enrol the new id (`pager peer rebind` there)
// before it accepts anything from this store again.
//
// It is refused while mail to any peer has no final result. A message's
// original identity is the store id it was written under, and version 1 sends
// it under the id of the link that carries it — so pending mail re-sent after
// the change would look new to a peer that had already imported it, and arrive
// twice. Cancel it first: `pager relay new-id --force` does that, telling each
// sender.
func RotateStoreID(ctx context.Context, st *store.Store) (string, error) {
	var id string
	err := st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		var err error
		id, err = RelinkAll(ctx, c)
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// RelinkAll gives this store a new id and every peer a fresh link with nothing
// on it, on the caller's write transaction, and returns the new id.
//
// It refuses while any outbound row has no final result, rather than trusting
// its caller to have closed them: a row left open would be re-sent under the
// new id and look new to a peer that already imported it. That refusal is
// RotateStoreID's whole answer to pending mail; ForceRotateStoreID closes the
// rows first in the same transaction.
func RelinkAll(ctx context.Context, c *sql.Conn) (string, error) {
	var pending int
	if err := c.QueryRowContext(ctx,
		"SELECT count(*) FROM messages WHERE dst_peer IS NOT NULL AND relayed_at IS NULL").Scan(&pending); err != nil {
		return "", fmt.Errorf("read pending mail: %w", err)
	}
	if pending > 0 {
		return "", fmt.Errorf("%w: %d message(s) to peers have no final result; "+
			"relay them first, or use --force to cancel them", ErrPeerHasPendingMail, pending)
	}
	id, err := newLinkID() // a store id has the same shape and source
	if err != nil {
		return "", err
	}
	if _, err := c.ExecContext(ctx,
		"UPDATE store_identity SET id = ? WHERE singleton = 1", id); err != nil {
		return "", fmt.Errorf("replace store id: %w", err)
	}
	rows, err := c.QueryContext(ctx, "SELECT name FROM peers ORDER BY name")
	if err != nil {
		return "", fmt.Errorf("read peers: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return "", err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, name := range names {
		link, err := newLinkID()
		if err != nil {
			return "", err
		}
		if _, err := c.ExecContext(ctx,
			"UPDATE peers SET link_id = ?, out_seq = 0 WHERE name = ?", link, name); err != nil {
			return "", fmt.Errorf("re-link peer %q: %w", name, err)
		}
	}
	return id, nil
}

// moveToLink renumbers the peer's mail that has no final result onto link,
// 1..n in the order it was numbered before, and returns n.
func moveToLink(ctx context.Context, c *sql.Conn, peer, link string) (int64, error) {
	rows, err := c.QueryContext(ctx,
		"SELECT id FROM messages WHERE dst_peer = ? AND relayed_at IS NULL ORDER BY dst_seq, id", peer)
	if err != nil {
		return 0, fmt.Errorf("read pending mail: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for i, id := range ids {
		if _, err := c.ExecContext(ctx,
			"UPDATE messages SET dst_link = ?, dst_seq = ? WHERE id = ?", link, i+1, id); err != nil {
			return 0, fmt.Errorf("renumber message %d: %w", id, err)
		}
	}
	return int64(len(ids)), nil
}

// PeerSeen is a peer as the roster footer shows it.
type PeerSeen struct {
	Name string
	// LastRelay is when this store last observed the link working — the later
	// of its last applied result and its last import — or 0 when it never has
	// (or the evidence has aged out with the mail).
	LastRelay int64
}

// PeersSeen lists the registered peers with what this store has observed of
// each. It is derived from the rows alone, so nothing — least of all relay
// out, which must stay a pure read — writes state for it. A cancellation is a
// local act and is not an observation of the peer, so it does not count.
func PeersSeen(ctx context.Context, st *store.Store) ([]PeerSeen, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT p.name,
		       max(COALESCE((SELECT max(m.relayed_at) FROM messages m
		                      WHERE m.dst_peer = p.name
		                        AND (m.relay_result IN ('imported', 'duplicate') OR m.relay_result LIKE 'rejected%')), 0),
		           COALESCE((SELECT max(m.created_at) FROM messages m WHERE m.src_peer = p.name), 0))
		  FROM peers p
		 ORDER BY p.name`)
	if err != nil {
		return nil, fmt.Errorf("read peer activity: %w", err)
	}
	defer rows.Close()
	var out []PeerSeen
	for rows.Next() {
		var p PeerSeen
		if err := rows.Scan(&p.Name, &p.LastRelay); err != nil {
			return nil, fmt.Errorf("read peer activity: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FormatPeerFooter is the line `pager who` and msg_roster add when peers are
// registered, or "" when there are none — so a store that never relays shows
// exactly what it always did. The roster does not list sessions in other
// stores; this says where they are and how their names are learned.
func FormatPeerFooter(peers []PeerSeen, now int64) string {
	if len(peers) == 0 {
		return ""
	}
	parts := make([]string, len(peers))
	for i, p := range peers {
		if p.LastRelay == 0 {
			parts[i] = p.Name + " (no relay seen)"
		} else {
			parts[i] = p.Name + " (last relay " + ago(now, p.LastRelay) + ", observed locally)"
		}
	}
	return "\npeers: " + strings.Join(parts, ", ") +
		" — address their sessions as name@peer; remote names are learned from mail they send you\n"
}
