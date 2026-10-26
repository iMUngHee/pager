package deliver

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iMUngHee/pager/internal/store"
)

func outSeq(t *testing.T, st *store.Store, peer string) int64 {
	t.Helper()
	var seq int64
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT out_seq FROM peers WHERE name = ?", peer).Scan(&seq); err != nil {
		t.Fatalf("read out_seq of %s: %v", peer, err)
	}
	return seq
}

func messageCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// TestOutboundRowNotDeliveredToLegacyAlias covers every query that joins mail
// to an inbox by alias. An outbound row is addressed "kipa@vm"; if a local
// alias with that exact spelling exists anyway — peer add refuses to create
// the situation, so it is seeded directly — the row must still be invisible to
// delivery, claiming, listing, the expired count, the inbox overview and the
// orphan hint. Otherwise it would be delivered here and relayed as well.
func TestOutboundRowNotDeliveredToLegacyAlias(t *testing.T) {
	st, fake := newStore(t)
	lim := DefaultLimits()
	mustAddPeer(t, st, "vm")

	addSession(t, st, "gone", workspace, "")
	legacyAlias(t, st, "zed@vm", "gone")
	old, err := Send(t.Context(), st, SendRequest{Alias: "zed@vm", Peer: "vm", Body: "for the orphan", Human: true})
	if err != nil {
		t.Fatalf("send to zed@vm: %v", err)
	}
	expired, err := Send(t.Context(), st, SendRequest{Alias: "kipa@vm", Peer: "vm", Body: "old", Human: true})
	if err != nil {
		t.Fatalf("send the old one: %v", err)
	}
	fake.Advance(lim.InjectTTL + time.Hour) // both rows are now past the delivery window; "gone" is stale

	addSession(t, st, "s1", workspace, "")
	legacyAlias(t, st, "kipa@vm", "s1")
	fresh, err := Send(t.Context(), st, SendRequest{Alias: "kipa@vm", Peer: "vm", Body: "fresh", Human: true})
	if err != nil {
		t.Fatalf("send the fresh one: %v", err)
	}

	pending, expiredCount, err := Candidates(t.Context(), st, "s1", lim)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(pending) != 0 || expiredCount != 0 {
		t.Errorf("Candidates = %d pending, %d expired; outbound rows must not count as mail here", len(pending), expiredCount)
	}

	if _, claimed, err := Claim(t.Context(), st, "s1", []int64{fresh.ID, expired.ID}, lim); err != nil || len(claimed) != 0 {
		t.Errorf("Claim of outbound rows = %v, %v; want nothing claimed", claimed, err)
	}

	listed, err := List(t.Context(), st, "s1", FilterAll, lim)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("List shows %d outbound rows as inbox mail", len(listed))
	}

	inboxes, err := Inboxes(t.Context(), st)
	if err != nil {
		t.Fatalf("Inboxes: %v", err)
	}
	for _, in := range inboxes {
		if strings.HasSuffix(in.Alias, "@vm") {
			t.Errorf("Inboxes lists %s with %d waiting", in.Alias, in.Waiting)
		}
	}

	orphans, err := OrphanAliases(t.Context(), st, workspace, tool, stale)
	if err != nil {
		t.Fatalf("OrphanAliases: %v", err)
	}
	for _, o := range orphans {
		if o.Alias == "zed@vm" {
			t.Errorf("OrphanAliases offers zed@vm for takeover with %d pending; message #%d is outbound", o.Pending, old.ID)
		}
	}
}

// TestImportsDoNotSpendHumanBudget checks that mail imported from another
// store is kept out of the store-wide human budget, which the local operator's
// --human sends share. The control half shows the same rows without src_peer
// do fill the budget, so the exclusion is what the result depends on.
func TestImportsDoNotSpendHumanBudget(t *testing.T) {
	seed := func(t *testing.T, st *store.Store, srcPeer any) {
		t.Helper()
		for i := 0; i < maxHumanPerWindow; i++ {
			if _, err := st.DB().ExecContext(t.Context(), `
				INSERT INTO messages(alias, body, hop, origin, created_at, src_peer, src_store, src_id)
				VALUES ('kipa', 'x', 1, 'human', ?, ?, ?, ?)`,
				st.Now(), srcPeer, nullable(peerStore(srcPeer)), i+1); err != nil {
				t.Fatalf("seed row %d: %v", i, err)
			}
		}
	}

	t.Run("local rows fill the budget", func(t *testing.T) {
		st, _ := newStore(t)
		seed(t, st, nil)
		if _, err := Send(t.Context(), st, SendRequest{Alias: "kipa", Body: "one more", Human: true}); !errors.Is(err, ErrBreakerOpen) {
			t.Fatalf("send after %d local human rows: err = %v, want ErrBreakerOpen", maxHumanPerWindow, err)
		}
	})

	t.Run("imported rows do not", func(t *testing.T) {
		st, _ := newStore(t)
		seed(t, st, "mac")
		if _, err := Send(t.Context(), st, SendRequest{Alias: "kipa", Body: "one more", Human: true}); err != nil {
			t.Fatalf("send after %d imported rows: %v", maxHumanPerWindow, err)
		}
	})
}

// peerStore gives seeded imported rows a store id, and local rows none.
func peerStore(srcPeer any) string {
	if srcPeer == nil {
		return ""
	}
	return otherStore
}

// TestSendRefusesOversizedRemoteBody pins where the body limit is enforced: at
// send time, before a sequence number exists. A number allocated to a message
// that is then refused would be a gap the receiving store waits on forever.
// The same holds for every other refusal, so a breaker refusal is checked too.
func TestSendRefusesOversizedRemoteBody(t *testing.T) {
	st, _ := newStore(t)
	mustAddPeer(t, st, "vm")

	_, err := Send(t.Context(), st, SendRequest{
		Alias: "kipa@vm", Peer: "vm", Body: strings.Repeat("x", MaxRelayBody+1), Human: true,
	})
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("oversized remote body: err = %v, want ErrBodyTooLarge", err)
	}
	if got := outSeq(t, st, "vm"); got != 0 {
		t.Errorf("out_seq = %d after a refused send, want 0", got)
	}
	if n := messageCount(t, st); n != 0 {
		t.Errorf("%d rows written by a refused send", n)
	}

	// A local message of the same size is not affected: the limit belongs to
	// the relay, not to local mail.
	if _, err := Send(t.Context(), st, SendRequest{Alias: "kipa", Body: strings.Repeat("x", MaxRelayBody+1), Human: true}); err != nil {
		t.Fatalf("local send of the same body: %v", err)
	}

	sent, err := Send(t.Context(), st, SendRequest{Alias: "kipa@vm", Peer: "vm", Body: strings.Repeat("x", MaxRelayBody), Human: true})
	if err != nil {
		t.Fatalf("remote body exactly at the limit: %v", err)
	}
	if got := outSeq(t, st, "vm"); got != 1 {
		t.Errorf("out_seq = %d after one accepted send, want 1", got)
	}
	var peer, link string
	var seq int64
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT dst_peer, dst_link, dst_seq FROM messages WHERE id = ?", sent.ID).Scan(&peer, &link, &seq); err != nil {
		t.Fatalf("read the outbound row: %v", err)
	}
	if peer != "vm" || seq != 1 || link == "" {
		t.Errorf("outbound row = (%q, %q, %d), want (vm, <link>, 1)", peer, link, seq)
	}

	// An address on a peer that is not registered fails without a row: the
	// sequence UPDATE finds nothing and the transaction rolls back.
	before := messageCount(t, st)
	if _, err := Send(t.Context(), st, SendRequest{Alias: "kipa@nowhere", Peer: "nowhere", Body: "x", Human: true}); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("send to an unregistered peer: err = %v, want ErrUnknownPeer", err)
	}
	if n := messageCount(t, st); n != before {
		t.Errorf("a refused send wrote %d rows", n-before)
	}

	// A breaker refusal must not consume a number either.
	for i := messageCount(t, st); i < maxHumanPerWindow; i++ {
		insertRaw(t, st, "filler", 0)
	}
	if _, err := Send(t.Context(), st, SendRequest{Alias: "kipa@vm", Peer: "vm", Body: "over budget", Human: true}); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("send over the human budget: err = %v, want ErrBreakerOpen", err)
	}
	if got := outSeq(t, st, "vm"); got != 1 {
		t.Errorf("out_seq = %d after a breaker refusal, want 1", got)
	}
}

// TestSendRefusesAddressOffItsPeer: a remote send whose address does not end
// in the peer it names is a caller bug, refused before anything is written.
func TestSendRefusesAddressOffItsPeer(t *testing.T) {
	st, _ := newStore(t)
	mustAddPeer(t, st, "vm")
	if _, err := Send(t.Context(), st, SendRequest{Alias: "kipa@lab", Peer: "vm", Body: "x", Human: true}); err == nil {
		t.Error("Send(kipa@lab, peer vm) succeeded")
	}
	if got := outSeq(t, st, "vm"); got != 0 {
		t.Errorf("out_seq = %d after a refused send, want 0", got)
	}
	if n := messageCount(t, st); n != 0 {
		t.Errorf("%d rows written", n)
	}
}

// TestSendRefusesOversizedRemoteLabelOrName: every field a relay record
// carries is bounded when the message is sent, so no record can outgrow the
// reader's line bound and block its link for good. Local mail is unaffected.
func TestSendRefusesOversizedRemoteLabelOrName(t *testing.T) {
	st, _ := newStore(t)
	mustAddPeer(t, st, "vm")
	if _, err := Send(t.Context(), st, SendRequest{
		Alias: "kipa@vm", Peer: "vm", Body: "x", Label: strings.Repeat("l", MaxRelayLabel+1), Human: true,
	}); err == nil {
		t.Error("an oversized label was accepted for another store")
	}
	if _, err := Send(t.Context(), st, SendRequest{
		Alias: strings.Repeat("n", MaxRelayName+1) + "@vm", Peer: "vm", Body: "x", Human: true,
	}); err == nil {
		t.Error("an oversized name was accepted for another store")
	}
	if got := outSeq(t, st, "vm"); got != 0 {
		t.Errorf("out_seq = %d after refused sends, want 0", got)
	}
	if _, err := Send(t.Context(), st, SendRequest{
		Alias: strings.Repeat("n", MaxRelayName) + "@vm", Peer: "vm", Body: "x", Label: strings.Repeat("l", MaxRelayLabel), Human: true,
	}); err != nil {
		t.Errorf("label and name exactly at their limits: %v", err)
	}
	if _, err := Send(t.Context(), st, SendRequest{Alias: "kipa", Body: "x", Label: strings.Repeat("l", MaxRelayLabel+1), Human: true}); err != nil {
		t.Errorf("a long label on local mail: %v", err)
	}
}
