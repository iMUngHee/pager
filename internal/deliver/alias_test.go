package deliver

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/iMUngHee/pager/internal/clock"
	"github.com/iMUngHee/pager/internal/store"
)

var testBase = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

const (
	workspace = "/tmp/workspace"
	tool      = "claude"
	stale     = store.DefaultStale
)

func newStore(t *testing.T) (*store.Store, *clock.Fake) {
	t.Helper()
	fake := clock.NewFake(testBase)
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), ".pager", "msg.db"), fake)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, fake
}

// addSession registers a session and heartbeats it at the clock's current time.
func addSession(t *testing.T, st *store.Store, id, root, pmRef string) {
	t.Helper()
	if err := st.RecordSession(t.Context(), store.SessionRecord{
		ID: id, Tool: tool, Root: root, PMRef: pmRef,
	}); err != nil {
		t.Fatalf("record session %s: %v", id, err)
	}
}

func mustSetAlias(t *testing.T, st *store.Store, alias, session string) {
	t.Helper()
	ok, err := SetAlias(t.Context(), st, alias, session)
	if err != nil {
		t.Fatalf("SetAlias(%s, %s): %v", alias, session, err)
	}
	if !ok {
		t.Fatalf("SetAlias(%s, %s) did not take effect", alias, session)
	}
}

// addPending queues an undelivered message for an alias.
func addPending(t *testing.T, st *store.Store, alias string) {
	t.Helper()
	if _, err := st.DB().ExecContext(t.Context(),
		"INSERT INTO messages(alias, body, hop, origin, created_at) VALUES (?, 'body', 0, 'human', ?)",
		alias, st.Now()); err != nil {
		t.Fatalf("queue message for %s: %v", alias, err)
	}
}

func aliasHolder(t *testing.T, st *store.Store, alias string) string {
	t.Helper()
	var holder string
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT COALESCE(session_id, '') FROM aliases WHERE alias = ?", alias).Scan(&holder); err != nil {
		t.Fatalf("read holder of %s: %v", alias, err)
	}
	return holder
}

func TestResolveTargetTierOrder(t *testing.T) {
	st, _ := newStore(t)
	ctx := t.Context()

	addSession(t, st, "session-alpha", workspace, "CORE/pager-core-delivery")
	addSession(t, st, "session-beta", workspace, "")
	mustSetAlias(t, st, "inbox", "session-alpha")
	mustSetAlias(t, st, "review-queue", "session-beta")

	for _, tc := range []struct{ name, ref, wantAlias string }{
		{"exact alias", "inbox", "inbox"},
		{"exact session id", "session-beta", "review-queue"},
		{"pm_ref suffix", "pager-core-delivery", "inbox"},
		{"full pm_ref", "CORE/pager-core-delivery", "inbox"},
		{"unique substring", "review", "review-queue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTarget(ctx, st, tc.ref, stale)
			if err != nil {
				t.Fatalf("ResolveTarget(%q): %v", tc.ref, err)
			}
			if got.Alias != tc.wantAlias {
				t.Errorf("alias = %q, want %q", got.Alias, tc.wantAlias)
			}
		})
	}
}

// TestResolveTargetExactBeatsSubstring is why the tiers are ordered. "in" is a
// complete alias and also a substring of another; the exact match must win
// rather than the reference being called ambiguous.
func TestResolveTargetExactBeatsSubstring(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "s1", workspace, "")
	mustSetAlias(t, st, "in", "s1")
	mustSetAlias(t, st, "inbox", "s1")

	got, err := ResolveTarget(t.Context(), st, "in", stale)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if got.Alias != "in" {
		t.Errorf("alias = %q, want the exact match %q", got.Alias, "in")
	}
}

func TestResolveTargetAmbiguous(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "s1", workspace, "")
	mustSetAlias(t, st, "review-a", "s1")
	mustSetAlias(t, st, "review-b", "s1")

	_, err := ResolveTarget(t.Context(), st, "review", stale)
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("err = %v, want an AmbiguousError", err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Errorf("candidates = %v, want both aliases", ambiguous.Candidates)
	}
}

// TestResolveTargetFoldsOneSession covers the regression automatic names would
// otherwise cause. Addressing a session by its id or its pm ref answers with
// every name that session holds, and once it holds two — the automatic one and
// one a person set — the old rule called that ambiguous and refused to deliver
// to a session that was never in doubt.
func TestResolveTargetFoldsOneSession(t *testing.T) {
	st, _ := newStore(t)
	ctx := t.Context()
	addSession(t, st, "s1", workspace, "CORE/some-item")

	if _, assigned, err := ensureAutoAlias(ctx, st, "s1", func(int) (string, error) { return "bavu", nil }); err != nil || !assigned {
		t.Fatalf("ensureAutoAlias: assigned %v, err %v", assigned, err)
	}
	mustSetAlias(t, st, "frontend", "s1")

	for _, ref := range []string{"s1", "some-item", "CORE/some-item"} {
		got, err := ResolveTarget(ctx, st, ref, stale)
		if err != nil {
			t.Fatalf("ResolveTarget(%q): %v", ref, err)
		}
		if got.SessionID != "s1" {
			t.Errorf("ResolveTarget(%q) session = %q, want %q", ref, got.SessionID, "s1")
		}
		if got.Alias != "frontend" {
			t.Errorf("ResolveTarget(%q) alias = %q, want the name the session is known by", ref, got.Alias)
		}
	}
}

// TestResolveTargetAmbiguousAcrossSessions is the boundary of that fold. Two
// sessions can carry the same pm ref, and collapsing those to one name would
// deliver to whichever happened to sort first — a wrong session, silently.
func TestResolveTargetAmbiguousAcrossSessions(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "s1", workspace, "CORE/shared")
	addSession(t, st, "s2", workspace, "CORE/shared")
	mustSetAlias(t, st, "one", "s1")
	mustSetAlias(t, st, "two", "s2")

	_, err := ResolveTarget(t.Context(), st, "shared", stale)
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("err = %v, want an AmbiguousError", err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Errorf("candidates = %v, want both sessions' names", ambiguous.Candidates)
	}
}

// TestResolveTargetSkipsDeadOnSubstring is the reason the live tier exists.
// Names are never reclaimed, so every session that ever ran leaves one behind,
// and without this the set a person picks from — what the roster shows — drifts
// away from the set a short reference is resolved against. An abbreviation that
// was unique when it was learned would start colliding with the name of a
// session that ended weeks ago.
func TestResolveTargetSkipsDeadOnSubstring(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()

	addSession(t, st, "departed", workspace, "")
	mustSetAlias(t, st, "fide", "departed")

	// Long enough that "departed" stops counting as reachable.
	fake.Advance(stale + time.Hour)
	addSession(t, st, "here", workspace, "")
	mustSetAlias(t, st, "config", "here")

	got, err := ResolveTarget(ctx, st, "fi", stale)
	if err != nil {
		t.Fatalf(`ResolveTarget("fi"): %v`, err)
	}
	if got.Alias != "config" {
		t.Errorf("alias = %q, want the live %q — the dead name must not compete", got.Alias, "config")
	}
}

// TestResolveTargetAmbiguousAmongLive fixes the boundary of that skip. Two live
// names matching one reference is a real question for the person to answer, and
// the candidate list is the answer they act on — so a name nobody is behind has
// no business being in it.
func TestResolveTargetAmbiguousAmongLive(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()

	addSession(t, st, "departed", workspace, "")
	mustSetAlias(t, st, "review-old", "departed")

	fake.Advance(stale + time.Hour)
	addSession(t, st, "s1", workspace, "")
	addSession(t, st, "s2", workspace, "")
	mustSetAlias(t, st, "review-a", "s1")
	mustSetAlias(t, st, "review-b", "s2")

	_, err := ResolveTarget(ctx, st, "review", stale)
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("err = %v, want an AmbiguousError", err)
	}
	for _, name := range ambiguous.Candidates {
		if name == "review-old" {
			t.Errorf("candidates = %v, want no name whose session is gone", ambiguous.Candidates)
		}
	}
	if len(ambiguous.Candidates) != 2 {
		t.Errorf("candidates = %v, want exactly the two live names", ambiguous.Candidates)
	}
}

// TestResolveTargetFallsBackToDead keeps the wider tier meaningful. Mail left
// for a name whose session ended is how a takeover starts — the message waits
// until someone claims the alias — so narrowing the search must not make those
// names unreachable, only outrankable.
func TestResolveTargetFallsBackToDead(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()

	addSession(t, st, "departed", workspace, "")
	mustSetAlias(t, st, "fide", "departed")
	fake.Advance(stale + time.Hour)

	got, err := ResolveTarget(ctx, st, "fid", stale)
	if err != nil {
		t.Fatalf(`ResolveTarget("fid"): %v`, err)
	}
	if got.Alias != "fide" {
		t.Errorf("alias = %q, want %q — with no live match the wider tier still answers", got.Alias, "fide")
	}
}

// TestResolveTargetExactReachesDead states the limit of the whole change: it
// touches how patterns are matched, never how a name is. Addressing a departed
// session by the name it was given is exactly what the orphan hint tells a
// person to do.
func TestResolveTargetExactReachesDead(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()

	addSession(t, st, "departed", workspace, "")
	mustSetAlias(t, st, "fide", "departed")

	fake.Advance(stale + time.Hour)
	addSession(t, st, "here", workspace, "")
	mustSetAlias(t, st, "config", "here")

	got, err := ResolveTarget(ctx, st, "fide", stale)
	if err != nil {
		t.Fatalf(`ResolveTarget("fide"): %v`, err)
	}
	if got.Alias != "fide" {
		t.Errorf("alias = %q, want the exact %q", got.Alias, "fide")
	}
}

// TestClaimTransfersAutoName: automatic names are never reclaimed by the
// system, but a person can still take one over. Mail addressed to a session
// that is gone would otherwise be stranded, since claiming is the only way to
// reach an inbox whose holder ended.
func TestClaimTransfersAutoName(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()
	addSession(t, st, "old", workspace, "")
	if _, assigned, err := ensureAutoAlias(ctx, st, "old", func(int) (string, error) { return "bavu", nil }); err != nil || !assigned {
		t.Fatalf("ensureAutoAlias: assigned %v, err %v", assigned, err)
	}
	addPending(t, st, "bavu")

	fake.Advance(stale + time.Hour)
	addSession(t, st, "new", workspace, "")

	ok, err := ClaimAlias(ctx, st, "bavu", "new", stale)
	if err != nil {
		t.Fatalf("ClaimAlias: %v", err)
	}
	if !ok {
		t.Fatal("an automatic name could not be claimed from a session that is gone")
	}
	if holder := aliasHolder(t, st, "bavu"); holder != "new" {
		t.Errorf("holder = %q, want %q", holder, "new")
	}
	if primary, err := PrimaryAlias(ctx, st, "new"); err != nil || primary != "bavu" {
		t.Errorf("primary alias of the claimant = %q (err %v), want %q", primary, err, "bavu")
	}

	waiting, _, err := Candidates(ctx, st, "new", DefaultLimits())
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(waiting) != 1 {
		t.Errorf("the claimant has %d messages waiting, want the 1 left behind", len(waiting))
	}
}

// TestPrunePreservesAutoName: names outlive the messages sent to them. Prune
// deletes messages only, and a name that disappeared with its last message
// would become free to hand to a different session — the surprise that
// automatic names are specifically built to avoid.
func TestPrunePreservesAutoName(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()
	addSession(t, st, "s1", workspace, "")
	name, _, err := ensureAutoAlias(ctx, st, "s1", func(int) (string, error) { return "bavu", nil })
	if err != nil {
		t.Fatalf("ensureAutoAlias: %v", err)
	}

	fake.Advance(store.DefaultRetention + time.Hour)
	if _, err := st.PruneNow(ctx, store.DefaultRetention, false); err != nil {
		t.Fatalf("PruneNow: %v", err)
	}

	if holder := aliasHolder(t, st, name); holder != "s1" {
		t.Errorf("after prune the name %q is held by %q, want %q", name, holder, "s1")
	}
}

func TestResolveTargetNoMatch(t *testing.T) {
	st, _ := newStore(t)
	if _, err := ResolveTarget(t.Context(), st, "nobody", stale); !errors.Is(err, ErrNoTarget) {
		t.Errorf("err = %v, want ErrNoTarget", err)
	}
}

func TestSetAliasIsIdempotentForItsOwner(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "s1", workspace, "")
	mustSetAlias(t, st, "inbox", "s1")
	mustSetAlias(t, st, "inbox", "s1") // must not fail
}

func TestSetAliasRefusesAnotherSessionsAlias(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "s1", workspace, "")
	addSession(t, st, "s2", workspace, "")
	mustSetAlias(t, st, "inbox", "s1")

	ok, err := SetAlias(t.Context(), st, "inbox", "s2")
	if err != nil {
		t.Fatalf("SetAlias: %v", err)
	}
	if ok {
		t.Error("a second session took the alias with SetAlias; that is ClaimAlias's job")
	}
	if got := aliasHolder(t, st, "inbox"); got != "s1" {
		t.Errorf("holder = %q, want %q", got, "s1")
	}
}

// TestNoAutomaticHandover is the misdelivery this design exists to prevent:
// matching workspace and a stale incumbent used to be enough to inherit an
// inbox, so today's session would silently receive yesterday's mail.
func TestNoAutomaticHandover(t *testing.T) {
	st, fake := newStore(t)
	addSession(t, st, "yesterday", workspace, "")
	mustSetAlias(t, st, "inbox", "yesterday")

	fake.Advance(stale + time.Hour) // yesterday's session goes stale
	addSession(t, st, "today", workspace, "")

	if got := aliasHolder(t, st, "inbox"); got != "yesterday" {
		t.Errorf("holder = %q, want it unchanged at %q — attaching must not inherit", got, "yesterday")
	}
}

func TestClaimAliasTakesOverStaleHolder(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()
	addSession(t, st, "old", workspace, "")
	mustSetAlias(t, st, "inbox", "old")

	before, err := LeaseGeneration(ctx, st, "inbox")
	if err != nil {
		t.Fatalf("LeaseGeneration: %v", err)
	}

	fake.Advance(stale + time.Hour)
	addSession(t, st, "new", workspace, "")

	ok, err := ClaimAlias(ctx, st, "inbox", "new", stale)
	if err != nil {
		t.Fatalf("ClaimAlias: %v", err)
	}
	if !ok {
		t.Fatal("claim of a stale alias failed")
	}
	if got := aliasHolder(t, st, "inbox"); got != "new" {
		t.Errorf("holder = %q, want %q", got, "new")
	}
	after, err := LeaseGeneration(ctx, st, "inbox")
	if err != nil {
		t.Fatalf("LeaseGeneration: %v", err)
	}
	if after != before+1 {
		t.Errorf("lease_generation = %d, want %d", after, before+1)
	}
}

// TestClaimRejectsLiveSession is the TOCTOU guard. The claimant observed the
// incumbent as stale, but the incumbent heartbeats before the claim lands. With
// the staleness test inside the UPDATE, the claim simply matches no rows.
func TestClaimRejectsLiveSession(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()
	addSession(t, st, "incumbent", workspace, "")
	mustSetAlias(t, st, "inbox", "incumbent")

	fake.Advance(stale + time.Hour)
	addSession(t, st, "claimant", workspace, "")

	// ... and the incumbent comes back to life between observation and claim.
	addSession(t, st, "incumbent", workspace, "")

	ok, err := ClaimAlias(ctx, st, "inbox", "claimant", stale)
	if err != nil {
		t.Fatalf("ClaimAlias: %v", err)
	}
	if ok {
		t.Error("claimed an alias whose holder is alive")
	}
	if got := aliasHolder(t, st, "inbox"); got != "incumbent" {
		t.Errorf("holder = %q, want %q", got, "incumbent")
	}
}

// TestClaimRejectsForeignWorkspace proves the isolation is the CAS's own doing
// — there is no separate workspace check to forget.
func TestClaimRejectsForeignWorkspace(t *testing.T) {
	st, fake := newStore(t)
	addSession(t, st, "old", workspace, "")
	mustSetAlias(t, st, "inbox", "old")

	fake.Advance(stale + time.Hour)
	addSession(t, st, "outsider", "/tmp/other-workspace", "")

	ok, err := ClaimAlias(t.Context(), st, "inbox", "outsider", stale)
	if err != nil {
		t.Fatalf("ClaimAlias: %v", err)
	}
	if ok {
		t.Error("a session in another workspace claimed the alias")
	}
}

// TestClaimRejectsStaleClaimant closes the other half: a session that is itself
// gone cannot collect an inbox on its way out.
func TestClaimRejectsStaleClaimant(t *testing.T) {
	st, fake := newStore(t)
	addSession(t, st, "old", workspace, "")
	mustSetAlias(t, st, "inbox", "old")
	addSession(t, st, "alsoOld", workspace, "")

	fake.Advance(stale + time.Hour) // both are now stale

	ok, err := ClaimAlias(t.Context(), st, "inbox", "alsoOld", stale)
	if err != nil {
		t.Fatalf("ClaimAlias: %v", err)
	}
	if ok {
		t.Error("a stale session claimed the alias")
	}
}

func TestClaimRejectsUnknownSession(t *testing.T) {
	st, fake := newStore(t)
	addSession(t, st, "old", workspace, "")
	mustSetAlias(t, st, "inbox", "old")
	fake.Advance(stale + time.Hour)

	ok, err := ClaimAlias(t.Context(), st, "inbox", "never-attached", stale)
	if err != nil {
		t.Fatalf("ClaimAlias: %v", err)
	}
	if ok {
		t.Error("a session that never attached claimed the alias")
	}
}

// TestConcurrentClaimAlias forces the race the CAS exists for: several live
// sessions all observe the same stale incumbent and claim at once.
func TestConcurrentClaimAlias(t *testing.T) {
	st, fake := newStore(t)
	const claimants = 8

	addSession(t, st, "old", workspace, "")
	mustSetAlias(t, st, "inbox", "old")
	fake.Advance(stale + time.Hour)
	for i := range claimants {
		addSession(t, st, claimantID(i), workspace, "")
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	won := make([]bool, claimants)
	errs := make([]error, claimants)
	for i := range claimants {
		wg.Go(func() {
			<-start
			won[i], errs[i] = ClaimAlias(context.Background(), st, "inbox", claimantID(i), stale)
		})
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("claimant %d: %v", i, err)
		}
		if won[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("%d claimants won, want exactly 1", winners)
	}

	gen, err := LeaseGeneration(t.Context(), st, "inbox")
	if err != nil {
		t.Fatalf("LeaseGeneration: %v", err)
	}
	if gen != 1 {
		t.Errorf("lease_generation = %d, want 1 — one takeover, not %d", gen, gen)
	}
}

func claimantID(i int) string { return "claimant-" + string(rune('a'+i)) }

// TestHintClaimedOnce covers concurrent hooks inside one session: the
// once-per-session orphan hint must be reserved, not merely checked.
func TestHintClaimedOnce(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "s1", workspace, "")

	const hooks = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	got := make([]bool, hooks)
	errs := make([]error, hooks)
	for i := range hooks {
		wg.Go(func() {
			<-start
			got[i], errs[i] = TakeClaimHint(context.Background(), st, "s1")
		})
	}
	close(start)
	wg.Wait()

	holders := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("hook %d: %v", i, err)
		}
		if got[i] {
			holders++
		}
	}
	if holders != 1 {
		t.Errorf("%d hooks showed the hint, want exactly 1", holders)
	}
}

// TestOrphanAliases pins the definition: gone holder *and* waiting mail. An
// alias whose session merely ended is not something to prompt about.
func TestOrphanAliases(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()

	addSession(t, st, "gone", workspace, "")
	addSession(t, st, "gone-quiet", workspace, "")
	mustSetAlias(t, st, "has-mail", "gone")
	mustSetAlias(t, st, "no-mail", "gone-quiet")
	addPending(t, st, "has-mail")

	fake.Advance(stale + time.Hour)

	// A live session in the same workspace, with mail, must not look orphaned.
	addSession(t, st, "live", workspace, "")
	mustSetAlias(t, st, "live-inbox", "live")
	addPending(t, st, "live-inbox")

	orphans, err := OrphanAliases(ctx, st, workspace, tool, stale)
	if err != nil {
		t.Fatalf("OrphanAliases: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans = %+v, want only has-mail", orphans)
	}
	if orphans[0].Alias != "has-mail" || orphans[0].Pending != 1 {
		t.Errorf("orphan = %+v, want {has-mail 1}", orphans[0])
	}
}

// TestOrphanAliasesIsolatedByWorkspace keeps a hint in one repository from
// advertising an inbox that belongs to another.
func TestOrphanAliasesIsolatedByWorkspace(t *testing.T) {
	st, fake := newStore(t)
	addSession(t, st, "elsewhere", "/tmp/other-workspace", "")
	mustSetAlias(t, st, "far-inbox", "elsewhere")
	addPending(t, st, "far-inbox")
	fake.Advance(stale + time.Hour)

	orphans, err := OrphanAliases(t.Context(), st, workspace, tool, stale)
	if err != nil {
		t.Fatalf("OrphanAliases: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphans = %+v, want none from another workspace", orphans)
	}
}
