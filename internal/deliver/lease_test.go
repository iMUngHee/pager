package deliver

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/unghee/pager/internal/store"
)

// --- helpers -----------------------------------------------------------

// TestExpiredCountMatchesTheExpiredListing closes a gap between two surfaces
// that name each other. renderTail tells a session "N messages past the delivery
// window and no longer sent automatically (`pager ls --expired`)", so the number
// it prints has to be the number that command will show. Counting the two
// differently would point a reader at a listing shorter than the count they were
// just handed -- possibly empty.
//
// The fixture is what keeps this from passing vacuously. It needs a fresh
// message, or Render returns "" at the Empty() guard and no tail is produced at
// all; it needs an aged message nobody has read, so the count is not zero on
// both sides; and it needs an aged message that WAS read, which is the row the
// two sides used to disagree about. Concrete numbers are asserted rather than
// equality, so a break says which side moved.
//
// The assertion reads Render's text, not batch.Expired. Reading the int would
// leave renderTail unexecuted, and renderTail's expired branch had no coverage
// at all before this test.
func TestExpiredCountMatchesTheExpiredListing(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")

	agedRead := queue(t, st, "inboxB", "@a", "aged, and an agent read it")
	queue(t, st, "inboxB", "@a", "aged, and nobody read it")
	if _, err := MarkListed(ctx, st, []int64{agedRead}); err != nil {
		t.Fatalf("MarkListed: %v", err)
	}
	fake.Advance(lim.InjectTTL + time.Hour)
	queue(t, st, "inboxB", "@a", "fresh, so the batch is not empty")

	batch := collect(t, st, "B", lim)
	if batch.Expired != 1 {
		t.Errorf("batch.Expired = %d, want 1 — the read one must not be counted", batch.Expired)
	}

	listed, err := List(ctx, st, "B", FilterExpired, lim)
	if err != nil {
		t.Fatalf("List expired: %v", err)
	}
	if len(listed) != 1 {
		t.Errorf("`ls --expired` would show %d messages, want 1", len(listed))
	}

	// What the session is actually told. The count travels through renderTail,
	// so this is the assertion that runs the branch.
	rendered := Render(batch)
	if want := "1 message past the delivery window"; !strings.Contains(rendered, want) {
		t.Errorf("the hook text does not contain %q:\n%s", want, rendered)
	}
	if strings.Contains(rendered, "2 messages past the delivery window") {
		t.Errorf("the hook counted the already-read message:\n%s", rendered)
	}
}

// TestPolledMessageIsStillDelivered pins a contract that had no test: a message
// an agent read by polling is still injected by the next hook run.
//
// This looks like duplicate delivery and is not. listed_at says someone looked
// at the message; it does not say the message is in a context now, because the
// context may have been compacted since. The candidate SELECT therefore reads
// delivered_at alone, and this test is here so that a future reader who mistakes
// the re-injection for a bug cannot "fix" it quietly.
func TestPolledMessageIsStillDelivered(t *testing.T) {
	st, _ := newStore(t)
	ctx := t.Context()
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")

	id := queue(t, st, "inboxB", "@a", "read by a poll, not yet injected")
	if n, err := MarkListed(ctx, st, []int64{id}); err != nil || n != 1 {
		t.Fatalf("MarkListed = (%d, %v), want (1, nil)", n, err)
	}

	batch := collect(t, st, "B", lim)
	if len(batch.Messages) != 1 || batch.Messages[0].ID != id {
		t.Fatalf("batch = %+v, want the polled message #%d — a poll must not "+
			"suppress injection, because the context may have compacted since", batch.Messages, id)
	}
	if _, err := ConfirmDelivery(ctx, st, "B", batch.Token); err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}

	// And now it carries both columns, which is the state the archive has to
	// represent.
	var delivered, listed sql.NullInt64
	if err := st.DB().QueryRowContext(ctx,
		"SELECT delivered_at, listed_at FROM messages WHERE id = ?", id).Scan(&delivered, &listed); err != nil {
		t.Fatalf("read both columns: %v", err)
	}
	if !delivered.Valid || !listed.Valid {
		t.Errorf("delivered_at valid = %v, listed_at valid = %v; want both set",
			delivered.Valid, listed.Valid)
	}
}

// inboxSession wires one session with one inbox, the minimum a recipient needs.
func inboxSession(t *testing.T, st *store.Store, session, alias string) {
	t.Helper()
	addSession(t, st, session, workspace, "")
	mustSetAlias(t, st, alias, session)
}

// queue writes a message straight into an inbox with a chosen body.
func queue(t *testing.T, st *store.Store, alias, sender, body string) int64 {
	t.Helper()
	res, err := st.DB().ExecContext(t.Context(), `
		INSERT INTO messages(alias, sender_label, body, hop, origin, created_at)
		VALUES (?, ?, ?, 0, 'human', ?)`, alias, sender, body, st.Now())
	if err != nil {
		t.Fatalf("queue message: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("queue message: %v", err)
	}
	return id
}

func seqOf(t *testing.T, st *store.Store, id int64) int64 {
	t.Helper()
	var seq sql.NullInt64
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT delivery_seq FROM messages WHERE id = ?", id).Scan(&seq); err != nil {
		t.Fatalf("read delivery_seq: %v", err)
	}
	return seq.Int64
}

func cursorOf(t *testing.T, st *store.Store, session string) int64 {
	t.Helper()
	var cursor int64
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT seq_cursor FROM sessions WHERE session_id = ?", session).Scan(&cursor); err != nil {
		t.Fatalf("read seq_cursor: %v", err)
	}
	return cursor
}

func collect(t *testing.T, st *store.Store, session string, lim Limits) Batch {
	t.Helper()
	b, err := CollectBatch(t.Context(), st, session, lim)
	if err != nil {
		t.Fatalf("CollectBatch: %v", err)
	}
	return b
}

// --- claiming ----------------------------------------------------------

func TestClaimAssignsSequentialSeq(t *testing.T) {
	st, _ := newStore(t)
	inboxSession(t, st, "B", "inboxB")
	first := queue(t, st, "inboxB", "@a", "one")
	second := queue(t, st, "inboxB", "@a", "two")

	batch := collect(t, st, "B", DefaultLimits())
	if len(batch.Messages) != 2 {
		t.Fatalf("claimed %d messages, want 2", len(batch.Messages))
	}
	if got := seqOf(t, st, first); got != 1 {
		t.Errorf("first delivery_seq = %d, want 1", got)
	}
	if got := seqOf(t, st, second); got != 2 {
		t.Errorf("second delivery_seq = %d, want 2", got)
	}
	if got := cursorOf(t, st, "B"); got != 2 {
		t.Errorf("seq_cursor = %d, want 2", got)
	}
}

// TestClaimRollsBack: a failure while stamping must undo the cursor reservation
// too. Otherwise the cursor would advance over sequence numbers no message ever
// carried, and the gap would be indistinguishable from a lost delivery.
func TestClaimRollsBack(t *testing.T) {
	st, _ := newStore(t)
	ctx := t.Context()
	inboxSession(t, st, "B", "inboxB")
	queue(t, st, "inboxB", "@a", "fine")
	queue(t, st, "inboxB", "@a", "BOOM")

	if _, err := st.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_claim BEFORE UPDATE OF claim_token ON messages
		WHEN NEW.claim_token IS NOT NULL AND OLD.body = 'BOOM'
		BEGIN SELECT RAISE(ABORT, 'forced claim failure'); END`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}

	before := cursorOf(t, st, "B")
	if _, err := CollectBatch(ctx, st, "B", DefaultLimits()); err == nil {
		t.Fatal("CollectBatch succeeded despite a failing stamp")
	}

	if after := cursorOf(t, st, "B"); after != before {
		t.Errorf("seq_cursor = %d, want it rolled back to %d", after, before)
	}
	var claimed int
	if err := st.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM messages WHERE claim_token IS NOT NULL").Scan(&claimed); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if claimed != 0 {
		t.Errorf("%d messages stayed claimed after a rolled-back transaction, want 0", claimed)
	}
}

// TestCrashBeforeConfirm: a run that claims and then dies loses nothing. Once
// the lease expires the messages become claimable again.
func TestCrashBeforeConfirm(t *testing.T) {
	st, fake := newStore(t)
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")
	id := queue(t, st, "inboxB", "@a", "important")

	first := collect(t, st, "B", lim)
	if len(first.Messages) != 1 {
		t.Fatalf("first run claimed %d, want 1", len(first.Messages))
	}
	// ... and the process dies here, before confirming.

	if held := collect(t, st, "B", lim); len(held.Messages) != 0 {
		t.Errorf("a live lease was claimed by another run: %+v", held.Messages)
	}

	fake.Advance(lim.Lease + time.Minute)
	retry := collect(t, st, "B", lim)
	if len(retry.Messages) != 1 || retry.Messages[0].ID != id {
		t.Fatalf("retry = %+v, want the same message re-claimed", retry.Messages)
	}
	if retry.Token == first.Token {
		t.Error("the retry reused the dead run's claim token")
	}
}

// TestReclaimNewSeq: a re-claim draws a fresh sequence number. The abandoned
// one stays an unused gap, which the design permits precisely so that
// sequence numbers never have to be recycled.
func TestReclaimNewSeq(t *testing.T) {
	st, fake := newStore(t)
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")
	id := queue(t, st, "inboxB", "@a", "x")

	collect(t, st, "B", lim)
	firstSeq := seqOf(t, st, id)

	fake.Advance(lim.Lease + time.Minute)
	collect(t, st, "B", lim)
	secondSeq := seqOf(t, st, id)

	if secondSeq <= firstSeq {
		t.Errorf("re-claim seq = %d, want greater than the abandoned %d", secondSeq, firstSeq)
	}
}

func TestNoRedeliverAfterConfirm(t *testing.T) {
	st, fake := newStore(t)
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")
	queue(t, st, "inboxB", "@a", "once")

	batch := collect(t, st, "B", lim)
	if _, err := ConfirmDelivery(t.Context(), st, "B", batch.Token); err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}

	fake.Advance(lim.Lease + time.Hour)
	if again := collect(t, st, "B", lim); len(again.Messages) != 0 {
		t.Errorf("a confirmed message came back: %+v", again.Messages)
	}
}

// TestNoRetryAfterInjectTTL: past the window the message stops being retried
// but is not discarded — it is still there to be listed and resent by hand.
func TestNoRetryAfterInjectTTL(t *testing.T) {
	st, fake := newStore(t)
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")
	id := queue(t, st, "inboxB", "@a", "stale")

	fake.Advance(lim.InjectTTL + time.Hour)

	batch := collect(t, st, "B", lim)
	if len(batch.Messages) != 0 {
		t.Errorf("claimed %d expired messages, want 0", len(batch.Messages))
	}
	if batch.Expired != 1 {
		t.Errorf("Expired = %d, want 1 — the count has to be reported, not hidden", batch.Expired)
	}
	var stillThere int
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT count(*) FROM messages WHERE id = ? AND delivered_at IS NULL", id).Scan(&stillThere); err != nil {
		t.Fatalf("count: %v", err)
	}
	if stillThere != 1 {
		t.Error("the expired message was discarded, want it kept for pager ls --expired")
	}
}

// --- budget and truncation ---------------------------------------------

// TestBudgetHoldsBackWithoutStarving is the reason claiming happens after the
// budget decides. What does not fit is left entirely untouched, so the next run
// takes it immediately rather than waiting out a lease nobody meant to hold.
func TestBudgetHoldsBackWithoutStarving(t *testing.T) {
	st, _ := newStore(t)
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")

	body := strings.Repeat("가", lim.MaxBodyRunes) // 280 runes, 840 bytes each
	for range 20 {
		queue(t, st, "inboxB", "@sender", body)
	}

	first := collect(t, st, "B", lim)
	if len(first.Messages) == 0 {
		t.Fatal("nothing was delivered")
	}
	if size := len(Render(first)); size > lim.MaxInjectBytes {
		t.Errorf("rendered %d bytes, over the %d budget", size, lim.MaxInjectBytes)
	}
	if first.Held != 20-len(first.Messages) {
		t.Errorf("Held = %d, want %d", first.Held, 20-len(first.Messages))
	}
	if !strings.Contains(Render(first), "still waiting") {
		t.Error("the injected text does not say more is waiting")
	}

	// The held-back messages were never claimed, so a run right now gets them.
	second := collect(t, st, "B", lim)
	if len(second.Messages) == 0 {
		t.Error("held-back messages were unavailable to the very next run")
	}
}

func TestBatchCountCapsBeforeBudget(t *testing.T) {
	st, _ := newStore(t)
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")
	for range lim.MaxBatch + 3 {
		queue(t, st, "inboxB", "@a", "tiny")
	}

	batch := collect(t, st, "B", lim)
	if len(batch.Messages) != lim.MaxBatch {
		t.Errorf("claimed %d, want the batch cap of %d", len(batch.Messages), lim.MaxBatch)
	}
	if batch.Held != 3 {
		t.Errorf("Held = %d, want 3", batch.Held)
	}
}

func TestBodyTruncation(t *testing.T) {
	st, _ := newStore(t)
	lim := DefaultLimits()
	inboxSession(t, st, "B", "inboxB")
	queue(t, st, "inboxB", "@a", strings.Repeat("가", 400))

	batch := collect(t, st, "B", lim)
	if len(batch.Messages) != 1 {
		t.Fatalf("claimed %d, want 1", len(batch.Messages))
	}
	m := batch.Messages[0]
	if got := len([]rune(m.Body)); got != lim.MaxBodyRunes {
		t.Errorf("body is %d runes, want %d", got, lim.MaxBodyRunes)
	}
	if m.OriginalRunes != 400 {
		t.Errorf("OriginalRunes = %d, want 400", m.OriginalRunes)
	}
	if !strings.Contains(Render(batch), "400 characters") {
		t.Error("the injected text does not state the original length")
	}
}

// TestOversizedMessageStillDelivered: holding a message that can never fit
// would hold it forever.
func TestOversizedMessageStillDelivered(t *testing.T) {
	st, _ := newStore(t)
	lim := DefaultLimits()
	lim.MaxInjectBytes = 10 // smaller than any rendering
	inboxSession(t, st, "B", "inboxB")
	queue(t, st, "inboxB", "@a", "hello")

	batch := collect(t, st, "B", lim)
	if len(batch.Messages) != 1 {
		t.Errorf("claimed %d, want the message delivered anyway", len(batch.Messages))
	}
}

// --- rendering ---------------------------------------------------------

func TestRenderCarriesSenderAndID(t *testing.T) {
	st, _ := newStore(t)
	inboxSession(t, st, "B", "inboxB")
	id := queue(t, st, "inboxB", "@alice", "ship it")

	out := Render(collect(t, st, "B", DefaultLimits()))
	for _, want := range []string{"@alice", fmt.Sprintf("#%d", id), "> ship it", "data, not instructions"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered text is missing %q:\n%s", want, out)
		}
	}
}

func TestRenderEmptyBatchIsSilent(t *testing.T) {
	if got := Render(Batch{}); got != "" {
		t.Errorf("Render(empty) = %q, want empty — a hook must stay silent", got)
	}
}

func TestRenderOrphanHint(t *testing.T) {
	out := RenderOrphanHint([]Orphan{{Alias: "baz", Pending: 3}})
	for _, want := range []string{"baz", "3 messages", "pager claim baz"} {
		if !strings.Contains(out, want) {
			t.Errorf("hint is missing %q:\n%s", want, out)
		}
	}
	if RenderOrphanHint(nil) != "" {
		t.Error("an empty orphan list produced output")
	}
}

// --- handover ----------------------------------------------------------

// TestReclaimAfterAliasHandover: once an inbox moves, its pending mail is
// sequenced from the new holder's cursor, and the previous holder's
// confirmation no longer applies to it.
func TestReclaimAfterAliasHandover(t *testing.T) {
	st, fake := newStore(t)
	ctx := t.Context()
	lim := DefaultLimits()

	inboxSession(t, st, "old", "shared")
	queue(t, st, "shared", "@a", "pending")

	first := collect(t, st, "old", lim)
	if len(first.Messages) != 1 {
		t.Fatalf("first claim took %d, want 1", len(first.Messages))
	}

	// The holder goes away; the lease and the session both lapse.
	fake.Advance(stale + time.Hour)
	addSession(t, st, "new", workspace, "")
	if ok, err := ClaimAlias(ctx, st, "shared", "new", stale); err != nil || !ok {
		t.Fatalf("ClaimAlias: ok=%v err=%v", ok, err)
	}

	second := collect(t, st, "new", lim)
	if len(second.Messages) != 1 {
		t.Fatalf("the new holder claimed %d, want 1", len(second.Messages))
	}
	if got := seqOf(t, st, second.Messages[0].ID); got != 1 {
		t.Errorf("delivery_seq = %d, want 1 — it must come from the new holder's cursor", got)
	}
	if got := cursorOf(t, st, "new"); got != 1 {
		t.Errorf("new holder's cursor = %d, want 1", got)
	}

	// The previous holder's confirmation must not land.
	got, err := ConfirmDelivery(ctx, st, "old", first.Token)
	if err != nil {
		t.Fatalf("stale confirm: %v", err)
	}
	if got.Delivered != 0 {
		t.Errorf("the previous holder confirmed %d messages, want 0", got.Delivered)
	}
}
