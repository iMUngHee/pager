package deliver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/iMUngHee/pager/internal/store"
)

// Address is where a send goes, before it is resolved.
//
// A target without '@' is a local reference and keeps every resolution tier it
// always had. A target with '@' is either a legacy local alias that happens to
// contain one — matched by its exact name only — or name@peer, a session in
// another store. Nothing in between: a reference with '@' never reaches the
// substring tiers, because "kipa@vm" quietly matching a local "kipa@vm2" is
// mail delivered to the wrong machine.
type Address struct {
	// Ref is the local reference: the whole target for local mail, the part
	// before '@' for remote mail.
	Ref string
	// Peer is the registered peer a remote address names, or "" for local mail.
	Peer string
	// Exact marks a legacy local alias containing '@'. It must be resolved by
	// exact name, never by the wider tiers.
	Exact bool
}

// Remote reports whether the address names a session in another store.
func (a Address) Remote() bool { return a.Peer != "" }

// ParseAddress classifies a send target. It reads the store only for targets
// containing '@'.
func ParseAddress(ctx context.Context, st *store.Store, target string) (Address, error) {
	ref := strings.TrimSpace(target)
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return Address{Ref: ref}, nil
	}

	var legacy int
	if err := st.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM aliases WHERE alias = ?", ref).Scan(&legacy); err != nil {
		return Address{}, fmt.Errorf("resolve %q: %w", ref, err)
	}
	if legacy > 0 {
		return Address{Ref: ref, Exact: true}, nil
	}

	local, peer := ref[:at], strings.ToLower(ref[at+1:])
	var name string
	err := st.DB().QueryRowContext(ctx, "SELECT name FROM peers WHERE name = ?", peer).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return Address{}, fmt.Errorf("%w %q in %q: no store is registered under that name (pager peer ls)",
			ErrUnknownPeer, peer, ref)
	}
	if err != nil {
		return Address{}, fmt.Errorf("resolve %q: %w", ref, err)
	}
	if local == "" || strings.Contains(local, "@") {
		return Address{}, fmt.Errorf("%q is not a remote address: the part before @%s must be a single name", ref, peer)
	}
	return Address{Ref: local, Peer: peer}, nil
}
