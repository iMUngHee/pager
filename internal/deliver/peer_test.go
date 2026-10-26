package deliver

import (
	"errors"
	"testing"

	"github.com/iMUngHee/pager/internal/store"
)

// otherStore is a well-formed id that is not the test store's own.
const otherStore = "0123456789abcdef0123456789abcdef"

// legacyAlias writes an alias the way a store created before the '@' rule could
// have held it. SetAlias refuses such names now, so the row goes in directly.
func legacyAlias(t *testing.T, st *store.Store, alias, session string) {
	t.Helper()
	if _, err := st.DB().ExecContext(t.Context(),
		"INSERT INTO aliases(alias, session_id, root, tool, updated_at) VALUES (?, ?, ?, ?, ?)",
		alias, session, workspace, tool, st.Now()); err != nil {
		t.Fatalf("seed legacy alias %s: %v", alias, err)
	}
}

func mustAddPeer(t *testing.T, st *store.Store, name string) Peer {
	t.Helper()
	p, err := AddPeer(t.Context(), st, name, otherStore)
	if err != nil {
		t.Fatalf("AddPeer(%s): %v", name, err)
	}
	return p
}

// TestSetAliasRefusesAt pins the rule that keeps name@peer unambiguous: no new
// local alias may be spelled like a remote address.
func TestSetAliasRefusesAt(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "s1", workspace, "")

	for _, name := range []string{"kipa@vm", "  kipa@vm  ", "@vm", "a@b@c"} {
		ok, err := SetAlias(t.Context(), st, name, "s1")
		if !errors.Is(err, ErrAliasHasAt) {
			t.Errorf("SetAlias(%q) err = %v, want ErrAliasHasAt", name, err)
		}
		if ok {
			t.Errorf("SetAlias(%q) reported success", name)
		}
	}
	var n int
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT count(*) FROM aliases WHERE instr(alias, '@') > 0").Scan(&n); err != nil {
		t.Fatalf("count aliases: %v", err)
	}
	if n != 0 {
		t.Errorf("%d aliases containing '@' were written", n)
	}

	mustSetAlias(t, st, "kipa", "s1") // an ordinary name is unaffected
}

// TestUnknownPeerDoesNotFallThroughToSubstring is the misdelivery the address
// grammar exists to prevent: with no peer "vm", "kipa@vm" must not be answered
// by the substring tier with a local "kipa@vm2".
func TestUnknownPeerDoesNotFallThroughToSubstring(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "s1", workspace, "")
	legacyAlias(t, st, "kipa@vm2", "s1")

	// The trap is real: the wide tiers would match it.
	if got, err := ResolveTarget(t.Context(), st, "kipa@vm", stale); err != nil || got.Alias != "kipa@vm2" {
		t.Fatalf("precondition: ResolveTarget(kipa@vm) = %+v, %v — the substring tier no longer matches, so this test proves nothing", got, err)
	}

	if _, err := ParseAddress(t.Context(), st, "kipa@vm"); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("ParseAddress(kipa@vm) with no peer vm: err = %v, want ErrUnknownPeer", err)
	}

	// The legacy name itself still resolves, but only by its exact spelling.
	got, err := ParseAddress(t.Context(), st, "kipa@vm2")
	if err != nil || got != (Address{Ref: "kipa@vm2", Exact: true}) {
		t.Errorf("ParseAddress(kipa@vm2) = %+v, %v, want the exact legacy alias", got, err)
	}

	// A peer that an existing alias ends with is refused, so the legacy name
	// can never shadow it.
	if _, err := AddPeer(t.Context(), st, "VM2", otherStore); err == nil {
		t.Error("AddPeer(vm2) succeeded although kipa@vm2 would shadow it")
	}

	mustAddPeer(t, st, "vm")
	for _, target := range []string{"kipa@vm", "kipa@VM", " kipa@vm "} {
		got, err := ParseAddress(t.Context(), st, target)
		if err != nil || got != (Address{Ref: "kipa", Peer: "vm"}) {
			t.Errorf("ParseAddress(%q) = %+v, %v, want kipa on peer vm", target, got, err)
		}
	}
	for _, target := range []string{"@vm", "a@b@vm"} {
		if got, err := ParseAddress(t.Context(), st, target); err == nil {
			t.Errorf("ParseAddress(%q) = %+v, want an error", target, got)
		}
	}

	// Without '@' nothing changes: no store read, the reference passes through.
	if got, err := ParseAddress(t.Context(), st, "kip"); err != nil || got != (Address{Ref: "kip"}) {
		t.Errorf("ParseAddress(kip) = %+v, %v", got, err)
	}
}

// TestPeerRemoveRefusedWithPendingMail keeps outbound rows from being
// stranded: a peer with mail that has no final result cannot be forgotten.
func TestPeerRemoveRefusedWithPendingMail(t *testing.T) {
	st, _ := newStore(t)
	mustAddPeer(t, st, "vm")

	res, err := st.DB().ExecContext(t.Context(), `
		INSERT INTO messages(alias, body, hop, origin, created_at, dst_peer, dst_link, dst_seq)
		VALUES ('kipa@vm', 'waiting to leave', 0, 'human', ?, 'vm', 'link', 1)`, st.Now())
	if err != nil {
		t.Fatalf("seed outbound row: %v", err)
	}
	id, _ := res.LastInsertId()

	if err := RemovePeer(t.Context(), st, "vm"); !errors.Is(err, ErrPeerHasPendingMail) {
		t.Fatalf("RemovePeer with pending mail: err = %v, want ErrPeerHasPendingMail", err)
	}
	peers, err := ListPeers(t.Context(), st)
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if len(peers) != 1 || peers[0].Pending != 1 {
		t.Fatalf("after a refused remove: peers = %+v, want vm with 1 pending", peers)
	}

	if _, err := st.DB().ExecContext(t.Context(),
		"UPDATE messages SET relayed_at = ?, relay_result = 'imported' WHERE id = ?", st.Now(), id); err != nil {
		t.Fatalf("finalise outbound row: %v", err)
	}
	if err := RemovePeer(t.Context(), st, "vm"); err != nil {
		t.Fatalf("RemovePeer once nothing is pending: %v", err)
	}
	if peers, _ := ListPeers(t.Context(), st); len(peers) != 0 {
		t.Errorf("peers after remove = %+v, want none", peers)
	}
	if err := RemovePeer(t.Context(), st, "vm"); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("RemovePeer of a removed peer: err = %v, want ErrUnknownPeer", err)
	}
}
