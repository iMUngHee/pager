package deliver

import (
	"errors"
	"strings"
	"testing"
	"time"

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
	if peers, err := ListPeers(t.Context(), st); err != nil || len(peers) != 0 {
		t.Errorf("peers after remove = %+v, %v; want none", peers, err)
	}
	if err := RemovePeer(t.Context(), st, "vm"); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("RemovePeer of a removed peer: err = %v, want ErrUnknownPeer", err)
	}
}

// TestAddPeerRefusals covers the registrations AddPeer turns away: a name
// already taken, this store's own id, an id of the wrong shape, and names a
// transport could not safely put on a command line. A refused add writes
// nothing.
func TestAddPeerRefusals(t *testing.T) {
	st, _ := newStore(t)
	own, err := st.ID(t.Context())
	if err != nil {
		t.Fatalf("store id: %v", err)
	}
	mustAddPeer(t, st, "vm")

	for _, tc := range []struct{ name, store string }{
		{"vm", otherStore},   // taken
		{"lab", own},         // this store
		{"lab", "not-an-id"}, // malformed id
		{"lab", strings.ToUpper(otherStore[:31])}, // wrong length
		{"", otherStore},
		{strings.Repeat("a", maxPeerName+1), otherStore},
		{"-lab", otherStore},
		{"la b", otherStore},
		{"lab;rm", otherStore},
		{"läb", otherStore},
	} {
		if _, err := AddPeer(t.Context(), st, tc.name, tc.store); err == nil {
			t.Errorf("AddPeer(%q, %q) succeeded, want a refusal", tc.name, tc.store)
		}
	}
	if peers, err := ListPeers(t.Context(), st); err != nil || len(peers) != 1 {
		t.Errorf("peers after refusals = %+v, %v; want only vm", peers, err)
	}

	for _, ok := range []string{"lab", "LAB-2", "a", strings.Repeat("z", maxPeerName)} {
		if _, err := NormalizePeerName(ok); err != nil {
			t.Errorf("NormalizePeerName(%q): %v", ok, err)
		}
	}
}

// TestResolveExactIgnoresWiderTiers pins ResolveExact's own contract: the
// exact alias or nothing. A fragment that the wider tiers would accept is
// refused, which is what keeps a legacy alias containing '@' from being
// reached — or matched — by anything but its own spelling.
func TestResolveExactIgnoresWiderTiers(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "s1", workspace, "")
	legacyAlias(t, st, "old@box", "s1")

	if got, err := ResolveExact(t.Context(), st, "old@box"); err != nil || got.Alias != "old@box" || got.SessionID != "s1" {
		t.Errorf("ResolveExact(old@box) = %+v, %v", got, err)
	}
	if got, err := ResolveTarget(t.Context(), st, "old@bo", stale); err != nil || got.Alias != "old@box" {
		t.Fatalf("precondition: the wider tiers match old@bo to %+v, %v — this test proves nothing", got, err)
	}
	if got, err := ResolveExact(t.Context(), st, "old@bo"); !errors.Is(err, ErrNoTarget) {
		t.Errorf("ResolveExact(old@bo) = %+v, %v; want ErrNoTarget", got, err)
	}
}

// TestRosterPeerFooter: no footer at all without peers, so a store that never
// relays shows exactly what it did before; with peers, the last relay is what
// this store observed — an applied result or an import — and a cancellation,
// being a local act, never counts as one.
func TestRosterPeerFooter(t *testing.T) {
	st, fake := newStore(t)
	if peers, err := PeersSeen(t.Context(), st); err != nil || FormatPeerFooter(peers, st.Now()) != "" {
		t.Fatalf("footer without peers = %q (%v), want none", FormatPeerFooter(peers, st.Now()), err)
	}

	mustAddPeer(t, st, "vm")
	if _, err := AddPeer(t.Context(), st, "lab", "fedcba9876543210fedcba9876543210"); err != nil {
		t.Fatalf("add lab: %v", err)
	}
	if _, err := AddPeer(t.Context(), st, "mac", "00112233445566778899aabbccddeeff"); err != nil {
		t.Fatalf("add mac: %v", err)
	}
	now := st.Now()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.DB().ExecContext(t.Context(), q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO messages(alias, body, hop, origin, created_at, dst_peer, dst_link, dst_seq, relayed_at, relay_result)
	      VALUES ('kipa@vm', 'x', 0, 'human', ?, 'vm', 'l', 1, ?, 'imported')`, now, now)
	exec(`INSERT INTO messages(alias, body, hop, origin, created_at, dst_peer, dst_link, dst_seq, relayed_at, relay_result)
	      VALUES ('kipa@lab', 'x', 0, 'human', ?, 'lab', 'l', 1, ?, 'cancelled: peer removed (may still arrive)')`, now, now)
	exec(`INSERT INTO messages(alias, body, hop, origin, created_at, src_peer, src_store, src_id)
	      VALUES ('kipa', 'x', 0, 'human', ?, 'mac', 'x', 1)`, now-int64(2*time.Hour/time.Millisecond))
	fake.Advance(3 * time.Minute)

	peers, err := PeersSeen(t.Context(), st)
	if err != nil {
		t.Fatalf("PeersSeen: %v", err)
	}
	got := FormatPeerFooter(peers, st.Now())
	want := "\npeers: lab (no relay seen), mac (last relay 2h ago, observed locally), vm (last relay 3m ago, observed locally)" +
		" — address their sessions as name@peer; remote names are learned from mail they send you\n"
	if got != want {
		t.Errorf("footer\n got %q\nwant %q", got, want)
	}
}

// TestRebindRefusals: a rebind needs a registered peer and another store's
// well-formed id, and a refused rebind changes nothing.
func TestRebindRefusals(t *testing.T) {
	st, _ := newStore(t)
	own, _ := st.ID(t.Context())
	before := mustAddPeer(t, st, "vm")
	for _, tc := range []struct{ name, store string }{
		{"vm", own},
		{"vm", "not-an-id"},
		{"nope", otherStore},
	} {
		if _, err := RebindPeer(t.Context(), st, tc.name, tc.store); err == nil {
			t.Errorf("RebindPeer(%q, %q) succeeded", tc.name, tc.store)
		}
	}
	peers, err := ListPeers(t.Context(), st)
	if err != nil || len(peers) != 1 || peers[0].LinkID != before.LinkID || peers[0].StoreID != before.StoreID {
		t.Errorf("peers after refused rebinds = %+v, %v; want vm unchanged", peers, err)
	}
}
