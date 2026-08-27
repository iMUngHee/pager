package deliver

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/unghee/pager/internal/store"
)

// --- helpers -----------------------------------------------------------

func dbPath(t *testing.T, st *store.Store) string {
	t.Helper()
	var p string
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&p); err != nil {
		t.Fatalf("database path: %v", err)
	}
	return p
}

// claimBatch stands in for the lease claim that arrives in I6. It reserves a
// sequence range from the session's cursor and stamps the messages exactly as
// the allocator specifies, so the confirmation rules are exercised against
// realistic claim state.
func claimBatch(t *testing.T, st *store.Store, session, token string, ids []int64) {
	t.Helper()
	ctx := t.Context()
	slices.Sort(ids) // batch order is by id, so a replay is deterministic

	var epoch int64
	if err := st.DB().QueryRowContext(ctx,
		"SELECT causal_epoch FROM sessions WHERE session_id = ?", session).Scan(&epoch); err != nil {
		t.Fatalf("read epoch: %v", err)
	}

	var base int64
	if err := st.DB().QueryRowContext(ctx,
		"UPDATE sessions SET seq_cursor = seq_cursor + ? WHERE session_id = ? RETURNING seq_cursor",
		len(ids), session).Scan(&base); err != nil {
		t.Fatalf("reserve sequence range: %v", err)
	}

	for i, id := range ids {
		seq := base - int64(len(ids)) + int64(i) + 1
		if _, err := st.DB().ExecContext(ctx, `
			UPDATE messages SET claim_token = ?, claim_epoch = ?, delivery_seq = ?, claimed_at = ?
			 WHERE id = ?`, token, epoch, seq, st.Now(), id); err != nil {
			t.Fatalf("stamp claim on %d: %v", id, err)
		}
	}
}

func deliverAndConfirm(t *testing.T, st *store.Store, session, token string, ids ...int64) Confirmation {
	t.Helper()
	claimBatch(t, st, session, token, ids)
	got, err := ConfirmDelivery(t.Context(), st, session, token)
	if err != nil {
		t.Fatalf("ConfirmDelivery(%s): %v", token, err)
	}
	return got
}

// insertRaw writes a message with an exact hop, bypassing Send. Confirmation
// rules need batches with chosen depths that a legitimate chain could not
// produce in one step.
func insertRaw(t *testing.T, st *store.Store, alias string, hop int) int64 {
	t.Helper()
	res, err := st.DB().ExecContext(t.Context(), `
		INSERT INTO messages(alias, body, hop, origin, created_at) VALUES (?, 'body', ?, 'human', ?)`,
		alias, hop, st.Now())
	if err != nil {
		t.Fatalf("insert raw message: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("insert raw message: %v", err)
	}
	return id
}

func causalState(t *testing.T, st *store.Store, session string) (id sql.NullInt64, seq, epoch int64) {
	t.Helper()
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT last_inbound_id, last_inbound_seq, causal_epoch FROM sessions WHERE session_id = ?",
		session).Scan(&id, &seq, &epoch); err != nil {
		t.Fatalf("read causal state: %v", err)
	}
	return id, seq, epoch
}

func mustSend(t *testing.T, st *store.Store, req SendRequest) Sent {
	t.Helper()
	sent, err := Send(t.Context(), st, req)
	if err != nil {
		t.Fatalf("Send to %s: %v", req.Alias, err)
	}
	return sent
}

// twoSessions wires A and B with an inbox each, the minimum for a chain.
func twoSessions(t *testing.T, st *store.Store) {
	t.Helper()
	addSession(t, st, "A", workspace, "")
	addSession(t, st, "B", workspace, "")
	mustSetAlias(t, st, "inboxA", "A")
	mustSetAlias(t, st, "inboxB", "B")
}

// --- send and depth ----------------------------------------------------

func TestSendRefusesWithoutContext(t *testing.T) {
	st, _ := newStore(t)
	_, err := Send(t.Context(), st, SendRequest{Alias: "inbox", Body: "hi"})
	if !errors.Is(err, ErrUnattributed) {
		t.Fatalf("err = %v, want ErrUnattributed", err)
	}

	var n int
	if err := st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d messages were written by a refused send, want 0", n)
	}
}

func TestSendHumanAssertionNeedsNoSession(t *testing.T) {
	st, _ := newStore(t)
	sent := mustSend(t, st, SendRequest{Alias: "inbox", Body: "hi", Human: true})
	if sent.Hop != 0 || sent.Origin != OriginHuman {
		t.Errorf("sent = %+v, want hop 0 and origin %q", sent, OriginHuman)
	}
}

func TestSendRefusesUnknownSession(t *testing.T) {
	st, _ := newStore(t)
	_, err := Send(t.Context(), st, SendRequest{Alias: "inbox", Body: "hi", Sender: "never-attached"})
	if !errors.Is(err, ErrUnattributed) {
		t.Errorf("err = %v, want ErrUnattributed", err)
	}
}

// TestHopChain walks a full ping-pong. Depth increases only because each reply
// is caused by a message the sender actually took delivery of.
func TestHopChain(t *testing.T) {
	st, _ := newStore(t)
	twoSessions(t, st)

	// A speaks first with nothing behind it, so the chain starts at zero.
	m0 := mustSend(t, st, SendRequest{Alias: "inboxB", Body: "0", Sender: "A"})
	if m0.Hop != 0 {
		t.Fatalf("first message hop = %d, want 0", m0.Hop)
	}
	deliverAndConfirm(t, st, "B", "t0", m0.ID)

	m1 := mustSend(t, st, SendRequest{Alias: "inboxA", Body: "1", Sender: "B"})
	if m1.Hop != 1 || m1.Origin != OriginCaused {
		t.Fatalf("reply = %+v, want hop 1 origin %q", m1, OriginCaused)
	}
	deliverAndConfirm(t, st, "A", "t1", m1.ID)

	m2 := mustSend(t, st, SendRequest{Alias: "inboxB", Body: "2", Sender: "A"})
	if m2.Hop != 2 {
		t.Fatalf("hop = %d, want 2", m2.Hop)
	}
	deliverAndConfirm(t, st, "B", "t2", m2.ID)

	m3 := mustSend(t, st, SendRequest{Alias: "inboxA", Body: "3", Sender: "B"})
	if m3.Hop != MaxHop {
		t.Fatalf("hop = %d, want %d", m3.Hop, MaxHop)
	}
	deliverAndConfirm(t, st, "A", "t3", m3.ID)

	// The fourth reply would be hop 4.
	if _, err := Send(t.Context(), st, SendRequest{Alias: "inboxB", Body: "4", Sender: "A"}); !errors.Is(err, ErrHopExceeded) {
		t.Fatalf("err = %v, want ErrHopExceeded", err)
	}
}

// TestHopRefusalNamesItsRemedy pins the sentence a blocked sender reads.
//
// It exists because the bare count was misread in the field: a session hit the
// limit, read "hop 4 exceeds the limit of 3", and told its operator that pager
// was cutting ordinary technical discussion short. That reading is wrong — what
// is capped is a chain of agent replies with no person in it — but nothing in
// the message said so, and an error whose correct response is "tell your user"
// has to say that or it gets reported as a bug instead of acted on.
//
// Pinned rather than screened, on the same reasoning as
// wake.TestDescribeDoesNotClaimThePeerActed: no test can read a sentence for
// its meaning, and a keyword screen passes rewordings that lose it. A pin
// cannot judge a reword either, but it brings every reword here, next to the
// reason the sentence exists.
func TestHopRefusalNamesItsRemedy(t *testing.T) {
	st, _ := newStore(t)
	twoSessions(t, st)

	deep := insertRaw(t, st, "inboxA", MaxHop)
	deliverAndConfirm(t, st, "A", "deep", deep)

	_, err := Send(t.Context(), st, SendRequest{Alias: "inboxB", Body: "one too many", Sender: "A"})
	if !errors.Is(err, ErrHopExceeded) {
		t.Fatalf("err = %v, want ErrHopExceeded", err)
	}

	const want = "causal chain is too deep: hop 4 exceeds the limit of 3 — " +
		"that many replies have passed with no person in them; " +
		"report it to your user rather than retrying, since their next prompt starts a fresh chain, " +
		"and --human is an operator's assertion rather than a way past this"
	if got := err.Error(); got != want {
		t.Errorf("Send error = %q\nwant %q\n"+
			"Reword freely, but only to something that still tells the reader to surface this "+
			"to a person, and still refuses --human as the way around it.", got, want)
	}
}

// TestHopResetsAfterUserPrompt is the escape hatch that keeps ordinary work
// from silting up: once the user speaks, the session's next send starts over.
func TestHopResetsAfterUserPrompt(t *testing.T) {
	st, _ := newStore(t)
	twoSessions(t, st)

	m0 := mustSend(t, st, SendRequest{Alias: "inboxB", Body: "0", Sender: "A", Human: true})
	deliverAndConfirm(t, st, "B", "t0", m0.ID)

	if got := mustSend(t, st, SendRequest{Alias: "inboxA", Body: "reply", Sender: "B"}); got.Hop != 1 {
		t.Fatalf("hop before reset = %d, want 1", got.Hop)
	}
	if err := ResetCausal(t.Context(), st, "B"); err != nil {
		t.Fatalf("ResetCausal: %v", err)
	}
	if got := mustSend(t, st, SendRequest{Alias: "inboxA", Body: "fresh", Sender: "B"}); got.Hop != 0 {
		t.Errorf("hop after reset = %d, want 0", got.Hop)
	}
}

// TestCausePrunedRejects covers the defence behind the prune exclusions: if a
// cause ever did become unreadable, the depth must be treated as beyond the
// limit. Reading it as zero would make waiting for retention a way to launder
// a chain back to the start.
func TestCausePrunedRejects(t *testing.T) {
	st, _ := newStore(t)
	twoSessions(t, st)

	m0 := mustSend(t, st, SendRequest{Alias: "inboxB", Body: "0", Sender: "A"})
	deliverAndConfirm(t, st, "B", "t0", m0.ID)

	// Foreign keys and the prune exclusions both prevent this state, so it has
	// to be forced through a connection that does not enforce them.
	raw, err := sql.Open("sqlite", "file:"+dbPath(t, st))
	if err != nil {
		t.Fatalf("open raw handle: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(t.Context(), "DELETE FROM messages WHERE id = ?", m0.ID); err != nil {
		t.Fatalf("force-delete the cause: %v", err)
	}

	_, err = Send(t.Context(), st, SendRequest{Alias: "inboxA", Body: "reply", Sender: "B"})
	if !errors.Is(err, ErrHopExceeded) {
		t.Fatalf("err = %v, want ErrHopExceeded for an unreadable cause", err)
	}
}

// --- confirmation and causal state -------------------------------------

// TestBatchCauseInheritance: with several messages arriving at once, the
// deepest one becomes the cause. Choosing a shallower one would let the chain
// regress and the ping-pong guard would never engage.
func TestBatchCauseInheritance(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "B", workspace, "")
	mustSetAlias(t, st, "inboxB", "B")

	shallow := insertRaw(t, st, "inboxB", 1)
	deepest := insertRaw(t, st, "inboxB", 3)
	middle := insertRaw(t, st, "inboxB", 2)

	got := deliverAndConfirm(t, st, "B", "batch", shallow, deepest, middle)
	if got.Delivered != 3 {
		t.Errorf("delivered = %d, want 3", got.Delivered)
	}
	if !got.CausalMoved {
		t.Error("causal state did not advance")
	}
	id, _, _ := causalState(t, st, "B")
	if id.Int64 != deepest {
		t.Errorf("last_inbound_id = %d, want the hop-3 message %d", id.Int64, deepest)
	}
}

// TestConfirmOutOfOrder rejects a late confirmation from an earlier batch
// without disturbing the state a later batch already established.
func TestConfirmOutOfOrder(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "B", workspace, "")
	mustSetAlias(t, st, "inboxB", "B")

	early := insertRaw(t, st, "inboxB", 0)
	late := insertRaw(t, st, "inboxB", 0)

	// Both are claimed, the later one taking the higher sequence.
	claimBatch(t, st, "B", "early", []int64{early})
	claimBatch(t, st, "B", "late", []int64{late})

	if got, err := ConfirmDelivery(t.Context(), st, "B", "late"); err != nil || !got.CausalMoved {
		t.Fatalf("confirming the later batch: %+v err=%v", got, err)
	}
	_, seqAfterLate, _ := causalState(t, st, "B")

	got, err := ConfirmDelivery(t.Context(), st, "B", "early")
	if err != nil {
		t.Fatalf("confirming the earlier batch: %v", err)
	}
	if got.CausalMoved {
		t.Error("an out-of-order confirmation moved the causal state")
	}
	if got.Delivered != 1 {
		t.Errorf("delivered = %d, want 1 — it still must not be redelivered", got.Delivered)
	}
	id, seq, _ := causalState(t, st, "B")
	if id.Int64 != late || seq != seqAfterLate {
		t.Errorf("causal state = (%d, %d), want it unchanged at (%d, %d)", id.Int64, seq, late, seqAfterLate)
	}
}

// TestNewChainAfterHighHop is the regression R4 found. Depth is not arrival
// order: after a chain ends at MaxHop, an unrelated message starting at zero
// must still become the causal representative. Ordering on depth left the
// session pinned to the deep message and computing every later send at hop 4.
func TestNewChainAfterHighHop(t *testing.T) {
	st, _ := newStore(t)
	twoSessions(t, st)

	deep := insertRaw(t, st, "inboxA", MaxHop)
	deliverAndConfirm(t, st, "A", "deep", deep)

	fresh := insertRaw(t, st, "inboxA", 0)
	got := deliverAndConfirm(t, st, "A", "fresh", fresh)
	if !got.CausalMoved {
		t.Fatal("a new chain's first message did not become the causal representative")
	}
	id, _, _ := causalState(t, st, "A")
	if id.Int64 != fresh {
		t.Fatalf("last_inbound_id = %d, want the new chain's message %d", id.Int64, fresh)
	}

	// And the session is not stuck: its next send is a reply to hop 0.
	sent := mustSend(t, st, SendRequest{Alias: "inboxB", Body: "reply", Sender: "A"})
	if sent.Hop != 1 {
		t.Errorf("hop = %d, want 1 — the session is still pinned to the deep chain", sent.Hop)
	}
}

// TestStaleConfirmAfterReset: a confirmation claimed before the user spoke must
// not revive the chain the user just walked away from.
func TestStaleConfirmAfterReset(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "B", workspace, "")
	mustSetAlias(t, st, "inboxB", "B")
	id := insertRaw(t, st, "inboxB", 1)

	claimBatch(t, st, "B", "inflight", []int64{id})
	if err := ResetCausal(t.Context(), st, "B"); err != nil {
		t.Fatalf("ResetCausal: %v", err)
	}

	got, err := ConfirmDelivery(t.Context(), st, "B", "inflight")
	if err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}
	if got.CausalMoved {
		t.Error("a confirmation from before the reset revived the chain")
	}
	last, seq, epoch := causalState(t, st, "B")
	if last.Valid {
		t.Errorf("last_inbound_id = %d, want NULL", last.Int64)
	}
	if seq != 0 {
		t.Errorf("last_inbound_seq = %d, want 0", seq)
	}
	if epoch != 1 {
		t.Errorf("causal_epoch = %d, want 1 after one reset", epoch)
	}
}

// TestStaleEpochConfirmMarksDelivered pins the other half of that outcome: the
// message was already written to the recipient, so it must not be redelivered
// even though it caused nothing.
func TestStaleEpochConfirmMarksDelivered(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "B", workspace, "")
	mustSetAlias(t, st, "inboxB", "B")
	id := insertRaw(t, st, "inboxB", 1)

	claimBatch(t, st, "B", "inflight", []int64{id})
	if err := ResetCausal(t.Context(), st, "B"); err != nil {
		t.Fatalf("ResetCausal: %v", err)
	}
	got, err := ConfirmDelivery(t.Context(), st, "B", "inflight")
	if err != nil {
		t.Fatalf("ConfirmDelivery: %v", err)
	}
	if got.Delivered != 1 {
		t.Errorf("delivered = %d, want 1", got.Delivered)
	}

	var deliveredAt sql.NullInt64
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT delivered_at FROM messages WHERE id = ?", id).Scan(&deliveredAt); err != nil {
		t.Fatalf("read message: %v", err)
	}
	if !deliveredAt.Valid {
		t.Error("delivered_at is NULL, so the message would be delivered again")
	}
}

// --- circuit breaker ---------------------------------------------------

// primeCause gives a session a causal representative so its sends count as
// automatic traffic.
func primeCause(t *testing.T, st *store.Store, session, alias string) {
	t.Helper()
	id := insertRaw(t, st, alias, 0)
	deliverAndConfirm(t, st, session, "prime-"+session, id)
}

func TestBreakerPerAliasPair(t *testing.T) {
	st, _ := newStore(t)
	twoSessions(t, st)
	primeCause(t, st, "A", "inboxA")

	for i := range maxCausedPerPair {
		if _, err := Send(t.Context(), st, SendRequest{Alias: "inboxB", Body: "x", Sender: "A"}); err != nil {
			t.Fatalf("automatic send %d: %v", i+1, err)
		}
	}
	_, err := Send(t.Context(), st, SendRequest{Alias: "inboxB", Body: "x", Sender: "A"})
	if !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("err = %v, want ErrBreakerOpen on send %d", err, maxCausedPerPair+1)
	}
	if !strings.Contains(err.Error(), "inboxB") {
		t.Errorf("error %q does not say which limit tripped", err)
	}
}

func TestBreakerPerSenderAcrossAliases(t *testing.T) {
	st, _ := newStore(t)
	twoSessions(t, st)
	primeCause(t, st, "A", "inboxA")

	// Spread across enough recipients that the per-pair limit never trips.
	aliases := make([]string, 0, 4)
	for i := range 4 {
		name := fmt.Sprintf("inbox%d", i)
		addSession(t, st, fmt.Sprintf("R%d", i), workspace, "")
		mustSetAlias(t, st, name, fmt.Sprintf("R%d", i))
		aliases = append(aliases, name)
	}

	for i := range maxCausedPerSender {
		alias := aliases[i%len(aliases)]
		if _, err := Send(t.Context(), st, SendRequest{Alias: alias, Body: "x", Sender: "A"}); err != nil {
			t.Fatalf("automatic send %d to %s: %v", i+1, alias, err)
		}
	}
	_, err := Send(t.Context(), st, SendRequest{Alias: aliases[0], Body: "x", Sender: "A"})
	if !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("err = %v, want ErrBreakerOpen on send %d", err, maxCausedPerSender+1)
	}
}

func TestBreakerHumanLimit(t *testing.T) {
	st, _ := newStore(t)
	for i := range maxHumanPerWindow {
		if _, err := Send(t.Context(), st, SendRequest{Alias: "inbox", Body: "x", Human: true}); err != nil {
			t.Fatalf("human send %d: %v", i+1, err)
		}
	}
	_, err := Send(t.Context(), st, SendRequest{Alias: "inbox", Body: "x", Human: true})
	if !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("err = %v, want ErrBreakerOpen on send %d", err, maxHumanPerWindow+1)
	}
}

// TestBreakerWindowExpires: the limit is a rate, not a quota.
func TestBreakerWindowExpires(t *testing.T) {
	st, fake := newStore(t)
	for range maxHumanPerWindow {
		if _, err := Send(t.Context(), st, SendRequest{Alias: "inbox", Body: "x", Human: true}); err != nil {
			t.Fatalf("human send: %v", err)
		}
	}
	if _, err := Send(t.Context(), st, SendRequest{Alias: "inbox", Body: "x", Human: true}); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("err = %v, want the breaker open", err)
	}

	fake.Advance(breakerWindow + time.Minute)
	if _, err := Send(t.Context(), st, SendRequest{Alias: "inbox", Body: "x", Human: true}); err != nil {
		t.Errorf("send after the window: %v", err)
	}
}
