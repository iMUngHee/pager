package deliver

import (
	"testing"
	"time"
)

// TestSenderLabelNeverBorrowsHuman is a live-testing find. A session with no
// alias used to be labelled "human", which is the one label an agent's message
// must never wear: --human is a claim the recipient acts on differently.
func TestSenderLabelNeverBorrowsHuman(t *testing.T) {
	st, _ := newStore(t)
	ctx := t.Context()
	addSession(t, st, "no-alias-session", workspace, "")
	addSession(t, st, "named-session", workspace, "")
	mustSetAlias(t, st, "named-box", "named-session")

	for _, tc := range []struct{ name, session, want string }{
		{"an alias is preferred", "named-session", "named-box"},
		{"without one, the session id", "no-alias-session", "no-alias-session"},
		{"human only when there is no session", "", "human"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SenderLabel(ctx, st, tc.session)
			if err != nil {
				t.Fatalf("SenderLabel: %v", err)
			}
			if got != tc.want {
				t.Errorf("SenderLabel(%q) = %q, want %q", tc.session, got, tc.want)
			}
		})
	}
}

// TestPrimaryAliasPrefersManual fixes the rule that lets an automatic name be
// an ordinary alias row: a name a person sets is the one the session is known
// by, however soon after the automatic one it was written.
//
// The clock is deliberately left where it starts. Both writes then carry the
// same millisecond, which is exactly the case that used to fall through to
// alphabetical order — and "bavu" sorts before "frontend", so the automatic
// name would win. The generator is injected for the same reason: a random name
// would pass this test roughly half the time by accident.
func TestPrimaryAliasPrefersManual(t *testing.T) {
	st, _ := newStore(t)
	ctx := t.Context()
	addSession(t, st, "s1", workspace, "")

	auto, assigned, err := ensureAutoAlias(ctx, st, "s1", func(int) (string, error) { return "bavu", nil })
	if err != nil || !assigned {
		t.Fatalf("ensureAutoAlias: name %q, assigned %v, err %v", auto, assigned, err)
	}
	mustSetAlias(t, st, "frontend", "s1")

	primary, err := PrimaryAlias(ctx, st, "s1")
	if err != nil {
		t.Fatalf("PrimaryAlias: %v", err)
	}
	if primary != "frontend" {
		t.Errorf("PrimaryAlias = %q, want the manually set %q", primary, "frontend")
	}
	label, err := SenderLabel(ctx, st, "s1")
	if err != nil {
		t.Fatalf("SenderLabel: %v", err)
	}
	if label != "frontend" {
		t.Errorf("SenderLabel = %q, want the manually set %q", label, "frontend")
	}

	// Re-running the same alias for its own session stays a successful no-op.
	mustSetAlias(t, st, "frontend", "s1")
	if primary, err = PrimaryAlias(ctx, st, "s1"); err != nil || primary != "frontend" {
		t.Errorf("after a repeat SetAlias: primary = %q, err %v", primary, err)
	}
}

// TestRosterNamesAndSkipsStale covers what the roster is for: it answers "who
// can I page", so a session that stopped heartbeating is not in it, and one
// that has no name yet is still shown by an address that works.
func TestRosterNamesAndSkipsStale(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()
	addSession(t, st, "named", workspace, "")
	addSession(t, st, "nameless", workspace, "")
	if _, _, err := ensureAutoAlias(ctx, st, "named", func(int) (string, error) { return "bavu", nil }); err != nil {
		t.Fatalf("ensureAutoAlias: %v", err)
	}

	fake.Advance(stale + time.Hour)
	addSession(t, st, "named", workspace, "")
	addSession(t, st, "nameless", workspace, "")
	// This one last heartbeat was a day ago and never came back.
	addSession(t, st, "gone", workspace, "")
	fake.Advance(stale + time.Hour)
	addSession(t, st, "named", workspace, "")
	addSession(t, st, "nameless", workspace, "")

	entries, err := Roster(ctx, st, stale)
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.SessionID] = e.Name
	}
	if _, listed := got["gone"]; listed {
		t.Error("the roster lists a session that stopped heartbeating")
	}
	if got["named"] != "bavu" {
		t.Errorf("named session shows as %q, want %q", got["named"], "bavu")
	}
	if got["nameless"] != "nameless" {
		t.Errorf("a session with no name shows as %q, want its session id", got["nameless"])
	}
}

func TestListReportsState(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")

	waiting := queue(t, st, "inboxB", "@a", "waiting")
	batch := collect(t, st, "B", lim)
	if _, err := ConfirmDelivery(ctx, st, "B", batch.Token); err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}
	delivered := waiting

	fake.Advance(lim.InjectTTL + time.Hour)
	expired := queue(t, st, "inboxB", "@a", "too late")
	fake.Advance(lim.InjectTTL + time.Hour)

	all, err := List(ctx, st, "B", false, lim)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	states := map[int64]Listed{}
	for _, m := range all {
		states[m.ID] = m
	}
	if !states[delivered].Delivered {
		t.Error("a confirmed message is not reported as delivered")
	}
	if states[expired].Delivered || !states[expired].Expired {
		t.Errorf("the aged message = %+v, want undelivered and expired", states[expired])
	}

	only, err := List(ctx, st, "B", true, lim)
	if err != nil {
		t.Fatalf("List expired: %v", err)
	}
	if len(only) != 1 || only[0].ID != expired {
		t.Errorf("expired-only listing = %+v, want just #%d", only, expired)
	}
}
