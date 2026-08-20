package deliver

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/unghee/pager/internal/store"
)

// stampListed sets listed_at directly.
//
// A filter test states its precondition as a row shape rather than routing
// through MarkListed: what the filter owes is an answer for a row that carries
// the column, however it came to carry it. Going through the writer instead
// would make these tests fail for two different reasons.
func stampListed(t *testing.T, st *store.Store, id int64) {
	t.Helper()
	if _, err := st.Exec(t.Context(),
		"UPDATE messages SET listed_at = ? WHERE id = ?", st.Now(), id); err != nil {
		t.Fatalf("stamp listed_at on #%d: %v", id, err)
	}
}

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

	all, err := List(ctx, st, "B", FilterAll, lim)
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

	only, err := List(ctx, st, "B", FilterExpired, lim)
	if err != nil {
		t.Fatalf("List expired: %v", err)
	}
	if len(only) != 1 || only[0].ID != expired {
		t.Errorf("expired-only listing = %+v, want just #%d", only, expired)
	}
}

// TestListFiltersByDeliveryState pins the cost contract: asking what is waiting
// must not carry the messages that have already been read.
//
// The three sets are asserted by id rather than by count, because the counts
// alone would also match a filter that returned the wrong rows — and waiting is
// asserted to CONTAIN expired, since the two nest rather than exclude each
// other.
func TestListFiltersByDeliveryState(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")

	// Delivered: queued, collected, confirmed.
	delivered := queue(t, st, "inboxB", "@a", "already read")
	batch := collect(t, st, "B", lim)
	if _, err := ConfirmDelivery(ctx, st, "B", batch.Token); err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}

	// Expired: queued, then aged past the injection window.
	expired := queue(t, st, "inboxB", "@a", "too late")
	fake.Advance(lim.InjectTTL + time.Hour)

	// Waiting: queued after the clock moved, so it is inside the window.
	fresh := queue(t, st, "inboxB", "@a", "still fresh")

	ids := func(f Filter) []int64 {
		t.Helper()
		got, err := List(ctx, st, "B", f, lim)
		if err != nil {
			t.Fatalf("List(%d): %v", f, err)
		}
		out := make([]int64, len(got))
		for i, m := range got {
			out[i] = m.ID
		}
		return out
	}

	for _, tc := range []struct {
		name   string
		filter Filter
		want   []int64
	}{
		{"all", FilterAll, []int64{delivered, expired, fresh}},
		{"waiting", FilterWaiting, []int64{expired, fresh}},
		{"expired", FilterExpired, []int64{expired}},
	} {
		if got := ids(tc.filter); !slices.Equal(got, tc.want) {
			t.Errorf("%s listing = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The nesting is the part a future change is most likely to break: narrowing
	// to waiting must not quietly drop the aged messages that still need resending.
	if got := ids(FilterWaiting); !slices.Contains(got, expired) {
		t.Errorf("waiting listing %v does not contain the expired message #%d", got, expired)
	}
}

// TestWaitingExcludesWhatWasAlreadyListed is the defect this change closes. A
// message read by a poll used to come back on every later poll, because the
// listing path recorded nothing and delivered_at only ever means "a hook
// injected this".
//
// The fixture is deliberately asymmetric: one row carries delivered_at alone and
// one carries listed_at alone. A fixture whose two columns are always NULL
// together cannot tell which half of the union does the work, so either clause
// could be deleted and the suite would stay green.
func TestWaitingExcludesWhatWasAlreadyListed(t *testing.T) {
	st, _ := newStore(t)
	ctx := t.Context()
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")

	// delivered_at only: queued, collected, confirmed.
	queue(t, st, "inboxB", "@a", "a hook injected this")
	batch := collect(t, st, "B", lim)
	if _, err := ConfirmDelivery(ctx, st, "B", batch.Token); err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}

	// listed_at only: never collected, so no hook has ever seen it.
	listed := queue(t, st, "inboxB", "@a", "an agent polled this")
	stampListed(t, st, listed)

	untouched := queue(t, st, "inboxB", "@a", "nobody has dealt with this")

	got, err := List(ctx, st, "B", FilterWaiting, lim)
	if err != nil {
		t.Fatalf("List waiting: %v", err)
	}
	ids := make([]int64, len(got))
	for i, m := range got {
		ids[i] = m.ID
	}
	if want := []int64{untouched}; !slices.Equal(ids, want) {
		t.Errorf("waiting listing = %v, want %v — only the row nobody has dealt with", ids, want)
	}
}

// TestExpiredExcludesWhatWasAlreadyListed carries the same union into the
// narrower filter, which is what keeps expired ⊂ waiting true.
//
// --expired means "automatic delivery gave up on this and it needs resending by
// hand". A message an agent already read by polling needs no resending, so it
// belongs in neither view — both are asserted here, because the nesting is what
// a change to one filter alone would break.
func TestExpiredExcludesWhatWasAlreadyListed(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")

	agedListed := queue(t, st, "inboxB", "@a", "aged, and an agent read it")
	agedUntouched := queue(t, st, "inboxB", "@a", "aged, and nobody read it")
	stampListed(t, st, agedListed)
	fake.Advance(lim.InjectTTL + time.Hour)

	ids := func(f Filter) []int64 {
		t.Helper()
		got, err := List(ctx, st, "B", f, lim)
		if err != nil {
			t.Fatalf("List(%d): %v", f, err)
		}
		out := make([]int64, len(got))
		for i, m := range got {
			out[i] = m.ID
		}
		return out
	}

	if want := []int64{agedUntouched}; !slices.Equal(ids(FilterExpired), want) {
		t.Errorf("expired listing = %v, want %v", ids(FilterExpired), want)
	}
	// The same row must be gone from the wider view too, or the nesting inverts:
	// a message would be expired without being waiting.
	if got := ids(FilterWaiting); slices.Contains(got, agedListed) {
		t.Errorf("waiting listing %v still carries the already-read message #%d", got, agedListed)
	}
}

// listedAt reads the raw column, so a test can tell "never stamped" from
// "stamped at some time" without going through a filter.
func listedAt(t *testing.T, st *store.Store, id int64) (int64, bool) {
	t.Helper()
	var v sql.NullInt64
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT listed_at FROM messages WHERE id = ?", id).Scan(&v); err != nil {
		t.Fatalf("read listed_at of #%d: %v", id, err)
	}
	return v.Int64, v.Valid
}

// TestMarkListedKeepsTheFirstSighting pins the listed_at IS NULL guard: the
// column answers "when did this first come into view", so a second listing must
// not move it.
//
// The clock is advanced between the two calls on purpose. clock.Fake only moves
// when Advance is called, so without it both writes would carry the same
// millisecond and the assertion would hold whether the guard existed or not —
// the test would be unable to fail.
func TestMarkListedKeepsTheFirstSighting(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()
	inboxSession(t, st, "B", "inboxB")
	id := queue(t, st, "inboxB", "@a", "read me twice")

	n, err := MarkListed(ctx, st, []int64{id})
	if err != nil {
		t.Fatalf("MarkListed: %v", err)
	}
	if n != 1 {
		t.Fatalf("first MarkListed changed %d rows, want 1", n)
	}
	first, ok := listedAt(t, st, id)
	if !ok {
		t.Fatal("listed_at is still NULL after the first listing")
	}

	fake.Advance(time.Hour)

	n, err = MarkListed(ctx, st, []int64{id})
	if err != nil {
		t.Fatalf("second MarkListed: %v", err)
	}
	if n != 0 {
		t.Errorf("second MarkListed changed %d rows, want 0 — the guard should have matched nothing", n)
	}
	if again, _ := listedAt(t, st, id); again != first {
		t.Errorf("listed_at moved from %d to %d; the first sighting is the one worth keeping", first, again)
	}
}

// TestMarkListedSkipsDeliveredMessages pins the delivered_at IS NULL clause,
// which is what gives listed_at its one meaning: an agent looked at this while
// it was still undelivered.
func TestMarkListedSkipsDeliveredMessages(t *testing.T) {
	st, _ := newStore(t)
	ctx := t.Context()
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")

	delivered := queue(t, st, "inboxB", "@a", "a hook already injected this")
	batch := collect(t, st, "B", lim)
	if _, err := ConfirmDelivery(ctx, st, "B", batch.Token); err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}
	undelivered := queue(t, st, "inboxB", "@a", "nothing has injected this")

	n, err := MarkListed(ctx, st, []int64{delivered, undelivered})
	if err != nil {
		t.Fatalf("MarkListed: %v", err)
	}
	if n != 1 {
		t.Errorf("MarkListed changed %d rows, want 1 — only the undelivered one", n)
	}
	if _, ok := listedAt(t, st, delivered); ok {
		t.Error("a message that was already delivered got a listed_at")
	}
	if _, ok := listedAt(t, st, undelivered); !ok {
		t.Error("the undelivered message did not get a listed_at")
	}
}

// TestMarkListedWithNoIDsWritesNothing pins the empty-slice guard. An empty
// IN () is a SQL syntax error, so the guard is required, and it belongs inside
// MarkListed the way Claim's does rather than in every caller.
func TestMarkListedWithNoIDsWritesNothing(t *testing.T) {
	st, _ := newStore(t)
	ctx := t.Context()
	inboxSession(t, st, "B", "inboxB")
	untouched := queue(t, st, "inboxB", "@a", "leave me alone")

	n, err := MarkListed(ctx, st, nil)
	if err != nil {
		t.Fatalf("MarkListed(nil): %v", err)
	}
	if n != 0 {
		t.Errorf("MarkListed(nil) changed %d rows, want 0", n)
	}
	if _, ok := listedAt(t, st, untouched); ok {
		t.Error("MarkListed(nil) stamped a row")
	}
}

// TestListingAloneRecordsNothing is the guard on Decision 1's boundary. `pager
// ls --session <id>` can list another session's inbox, so if the shared read
// path ever started stamping, a person glancing at a queue would consume an
// agent's mail. List must stay a pure read for every filter.
func TestListingAloneRecordsNothing(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")

	aged := queue(t, st, "inboxB", "@a", "aged")
	fake.Advance(lim.InjectTTL + time.Hour)
	fresh := queue(t, st, "inboxB", "@a", "fresh")

	for _, f := range []Filter{FilterAll, FilterWaiting, FilterExpired} {
		if _, err := List(ctx, st, "B", f, lim); err != nil {
			t.Fatalf("List(%d): %v", f, err)
		}
	}
	for _, id := range []int64{aged, fresh} {
		if _, ok := listedAt(t, st, id); ok {
			t.Errorf("listing alone stamped #%d — the read path must record nothing", id)
		}
	}
}

// TestStateLabelsBothAxes pins the STATE vocabulary across every combination of
// the two axes, and with it the promise the flag names rest on: what a filter
// returns must agree with the word the column printed.
//
// The invariant asserted per row is "filter X contains every row whose state is
// X". A label that named a state its own filter excluded would make `--waiting`
// a lie for the row it just printed as waiting, which is the incoherence the new
// label exists to prevent.
//
// delivered+seen is here deliberately: it is the combination Decision 8 accepts
// a loss on (the poll stops showing in the column) and the one an earlier draft
// of this plan got wrong by assuming it could not happen.
func TestStateLabelsBothAxes(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		delivered, seen, expired bool
		want                     string
		inWaiting                bool
	}{
		{name: "nobody has touched it", want: "waiting", inWaiting: true},
		{name: "aged and untouched", expired: true, want: "expired", inWaiting: true},
		{name: "an agent polled it", seen: true, want: "seen"},
		{name: "aged and polled", seen: true, expired: true, want: "seen"},
		{name: "a hook injected it", delivered: true, want: "delivered"},
		{name: "injected and polled", delivered: true, seen: true, want: "delivered"},
		{name: "injected, polled, aged", delivered: true, seen: true, expired: true, want: "delivered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := Listed{Delivered: tc.delivered, Seen: tc.seen, Expired: tc.expired}
			if got := l.State(); got != tc.want {
				t.Errorf("State() = %q, want %q", got, tc.want)
			}
			// A row is in the narrowing filters exactly when neither axis is set.
			// That is the union both of them apply, so a state of waiting or
			// expired must coincide with it and seen or delivered must not.
			undealtWith := !tc.delivered && !tc.seen
			if undealtWith != tc.inWaiting {
				t.Errorf("row is undealt-with = %v but the case says --waiting = %v; "+
					"the STATE column and the filter disagree", undealtWith, tc.inWaiting)
			}
		})
	}
}

// TestFilterFromPrefersExpired fixes the precedence rule. Expired is the
// intersection of the two flags, so applying it is what honours both.
func TestFilterFromPrefersExpired(t *testing.T) {
	for _, tc := range []struct {
		waiting, expired bool
		want             Filter
	}{
		{false, false, FilterAll},
		{true, false, FilterWaiting},
		{false, true, FilterExpired},
		{true, true, FilterExpired},
	} {
		if got := FilterFrom(tc.waiting, tc.expired); got != tc.want {
			t.Errorf("FilterFrom(waiting=%v, expired=%v) = %d, want %d",
				tc.waiting, tc.expired, got, tc.want)
		}
	}
}
