package deliver

import (
	"testing"

	"github.com/unghee/pager/internal/store"
)

// orphanAlias writes an alias whose holder is NULL, which no code path
// produces.
//
// An earlier version of this comment said the store reaches it "by an alias
// outliving its session". That is wrong, and pager-stale-holder-send-notice is
// the item that exists because it is wrong: an alias outliving its session
// keeps its session_id, which is exactly why a dead inbox still resolves and
// still looks ordinary. Both alias INSERTs take session_id from the sessions
// primary key, ClaimAlias only writes a non-empty session, nothing NULLs the
// column, and nothing deletes a session row.
//
// The LEFT JOIN this covers is still worth covering — the schema permits the
// state and a query has to answer for it — but do not read this helper as
// evidence the state occurs.
func orphanAlias(t *testing.T, st *store.Store, alias string) {
	t.Helper()
	if _, err := st.DB().ExecContext(t.Context(), `
		INSERT INTO aliases(alias, session_id, root, tool, updated_at)
		VALUES (?, NULL, ?, ?, ?)`, alias, workspace, tool, st.Now()); err != nil {
		t.Fatalf("write orphan alias %s: %v", alias, err)
	}
}

// bindHost gives a session a host process, which addSession deliberately does
// not: the roster never needed one, and an inbox listing is the first reader of
// those columns.
func bindHost(t *testing.T, st *store.Store, session string, pid int, start int64) {
	t.Helper()
	if _, err := st.DB().ExecContext(t.Context(),
		"UPDATE sessions SET host_client = ?, host_pid = ?, host_start = ? WHERE session_id = ?",
		tool, pid, start, session); err != nil {
		t.Fatalf("bind host to %s: %v", session, err)
	}
}

func inboxesByAlias(t *testing.T, st *store.Store) map[string]Inbox {
	t.Helper()
	got, err := Inboxes(t.Context(), st)
	if err != nil {
		t.Fatalf("Inboxes: %v", err)
	}
	out := make(map[string]Inbox, len(got))
	for _, i := range got {
		out[i.Alias] = i
	}
	return out
}

// TestInboxesCountsOnlyUndealtWithMail is the whole point of the listing: the
// count has to mean what `ls --waiting` means by waiting, on both axes.
//
// The fixture is asymmetric on purpose, the same way the filter tests are. One
// message carries delivered_at alone and one carries listed_at alone, so a
// query that dropped either half of undealtWith would report a different number
// here rather than staying green.
func TestInboxesCountsOnlyUndealtWithMail(t *testing.T) {
	st, _ := newStore(t)
	ctx := t.Context()
	lim := DefaultLimits()
	inboxSession(t, st, "A", "inboxA")
	inboxSession(t, st, "B", "inboxB")

	// inboxA: one delivered, one listed, two still waiting.
	queue(t, st, "inboxA", "@x", "already injected")
	batch := collect(t, st, "A", lim)
	if _, err := ConfirmDelivery(ctx, st, "A", batch.Token); err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}
	polled := queue(t, st, "inboxA", "@x", "read by a poll")
	stampListed(t, st, polled)
	queue(t, st, "inboxA", "@x", "waiting one")
	queue(t, st, "inboxA", "@x", "waiting two")

	// inboxB: one waiting, so it must appear with a count of its own rather
	// than inherit A's.
	queue(t, st, "inboxB", "@x", "waiting")

	// inboxC has nothing waiting at all and must not be listed.
	inboxSession(t, st, "C", "inboxC")
	dealt := queue(t, st, "inboxC", "@x", "seen already")
	stampListed(t, st, dealt)

	got := inboxesByAlias(t, st)
	for alias, want := range map[string]int{"inboxA": 2, "inboxB": 1} {
		if got[alias].Waiting != want {
			t.Errorf("%s waiting = %d, want %d", alias, got[alias].Waiting, want)
		}
	}
	if _, listed := got["inboxC"]; listed {
		t.Error("an inbox with nothing waiting is listed")
	}

	// The counts must agree with what the per-session listing reports, since a
	// disagreement is the defect this shares undealtWith to prevent.
	perSession, err := List(ctx, st, "A", FilterWaiting, lim)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(perSession) != got["inboxA"].Waiting {
		t.Errorf("ls --waiting reports %d for inboxA, the overview reports %d",
			len(perSession), got["inboxA"].Waiting)
	}
}

// TestInboxesKeepsInboxesWhoseHolderIsGone covers the case the listing exists
// to surface. Mail waiting in an inbox nobody is behind is not noise to drop —
// it is how a takeover gets started, and an inner join would hide it.
func TestInboxesKeepsInboxesWhoseHolderIsGone(t *testing.T) {
	st, _ := newStore(t)

	// Held, with a host process recorded.
	inboxSession(t, st, "live", "held")
	bindHost(t, st, "live", 4242, 99)
	queue(t, st, "held", "@x", "one")

	// Held, but host detection failed when the session attached.
	inboxSession(t, st, "hostless", "unbound")
	queue(t, st, "unbound", "@x", "one")

	// Not held at all: the alias outlived its session.
	orphanAlias(t, st, "stranded")
	queue(t, st, "stranded", "@x", "one")

	got := inboxesByAlias(t, st)
	for _, alias := range []string{"held", "unbound", "stranded"} {
		if _, listed := got[alias]; !listed {
			t.Fatalf("%s is not listed", alias)
		}
	}
	if held := got["held"]; held.HostPid != 4242 || held.HostStart != 99 || held.SessionID != "live" {
		t.Errorf("held = %+v, want session live at pid 4242 start 99", held)
	}
	// Zero rather than a stale value: nothing to ask about, which a renderer
	// must not mistake for a process that has exited.
	if unbound := got["unbound"]; unbound.HostPid != 0 || unbound.SessionID != "hostless" {
		t.Errorf("unbound = %+v, want session hostless with no host pid", unbound)
	}
	if stranded := got["stranded"]; stranded.HostPid != 0 || stranded.SessionID != "" {
		t.Errorf("stranded = %+v, want no holder and no host pid", stranded)
	}
}

// TestInboxesOrdersByAlias fixes the order because the intended consumer
// redraws once a second: anything count-ordered would reshuffle under the
// reader every time a message lands.
func TestInboxesOrdersByAlias(t *testing.T) {
	st, _ := newStore(t)
	// Inserted out of order, and with counts that would invert the result if
	// pressure decided it.
	for _, tc := range []struct {
		alias string
		count int
	}{{"zulu", 1}, {"alpha", 3}, {"mike", 2}} {
		inboxSession(t, st, "s-"+tc.alias, tc.alias)
		for range tc.count {
			queue(t, st, tc.alias, "@x", "waiting")
		}
	}

	got, err := Inboxes(t.Context(), st)
	if err != nil {
		t.Fatalf("Inboxes: %v", err)
	}
	var order []string
	for _, i := range got {
		order = append(order, i.Alias)
	}
	want := []string{"alpha", "mike", "zulu"}
	if len(order) != len(want) {
		t.Fatalf("listed %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("listed %v, want %v", order, want)
		}
	}
}
