package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iMUngHee/pager/internal/clock"
	"github.com/iMUngHee/pager/internal/deliver"
	"github.com/iMUngHee/pager/internal/store"
)

var testBase = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

// node is one store in a test: a laptop ("a", which knows the other as peer
// "vm") or a VM ("b", which knows the first as peer "mac").
type node struct {
	st   *store.Store
	fake *clock.Fake
	id   string
}

func newNode(t *testing.T) node {
	t.Helper()
	fake := clock.NewFake(testBase)
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), ".pager", "msg.db"), fake)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id, err := st.ID(t.Context())
	if err != nil {
		t.Fatalf("store id: %v", err)
	}
	return node{st: st, fake: fake, id: id}
}

// pair enrols two stores with each other.
func pair(t *testing.T) (a, b node) {
	t.Helper()
	a, b = newNode(t), newNode(t)
	if _, err := deliver.AddPeer(t.Context(), a.st, "vm", b.id); err != nil {
		t.Fatalf("a adds vm: %v", err)
	}
	if _, err := deliver.AddPeer(t.Context(), b.st, "mac", a.id); err != nil {
		t.Fatalf("b adds mac: %v", err)
	}
	return a, b
}

func session(t *testing.T, n node, id, alias string) {
	t.Helper()
	if err := n.st.RecordSession(t.Context(), store.SessionRecord{ID: id, Tool: "claude", Root: "/tmp/ws"}); err != nil {
		t.Fatalf("record %s: %v", id, err)
	}
	if alias != "" {
		if ok, err := deliver.SetAlias(t.Context(), n.st, alias, id); err != nil || !ok {
			t.Fatalf("alias %s: %v %v", alias, ok, err)
		}
	}
}

func sendRemote(t *testing.T, n node, sender, to, body string) deliver.Sent {
	t.Helper()
	label, err := deliver.SenderLabel(t.Context(), n.st, sender)
	if err != nil {
		t.Fatalf("label: %v", err)
	}
	sent, err := deliver.Send(t.Context(), n.st, deliver.SendRequest{
		Alias: to + "@vm", Peer: "vm", Body: body, Sender: sender, Label: label, Human: sender == "",
	})
	if err != nil {
		t.Fatalf("send to %s@vm: %v", to, err)
	}
	return sent
}

func out(t *testing.T, n node) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Out(t.Context(), n.st, "vm", DefaultLimit, &buf); err != nil {
		t.Fatalf("relay out: %v", err)
	}
	return buf.Bytes()
}

// in runs relay in and returns its results with the exit code (0 for nil).
func in(t *testing.T, n node, peer string, stream []byte) ([]Result, int) {
	t.Helper()
	var res bytes.Buffer
	err := In(t.Context(), n.st, peer, bytes.NewReader(stream), &res, io.Discard)
	return parseResults(t, res.Bytes()), exitCode(t, err)
}

func ack(t *testing.T, n node, results []byte) int {
	t.Helper()
	return exitCode(t, Ack(t.Context(), n.st, "vm", bytes.NewReader(results), io.Discard))
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	t.Fatalf("unexpected error: %v", err)
	return -1
}

func parseResults(t *testing.T, b []byte) []Result {
	t.Helper()
	var rs []Result
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r Result
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("parse result %q: %v", line, err)
		}
		rs = append(rs, r)
	}
	return rs
}

func lines(b []byte) [][]byte {
	var ls [][]byte
	for _, l := range bytes.SplitAfter(b, []byte("\n")) {
		if len(l) > 0 {
			ls = append(ls, l)
		}
	}
	return ls
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeLine(&buf, v); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func count(t *testing.T, n node, query string, args ...any) int {
	t.Helper()
	var c int
	if err := n.st.DB().QueryRowContext(t.Context(), query, args...).Scan(&c); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return c
}

func inSeq(t *testing.T, n node, link string) int64 {
	t.Helper()
	var s int64
	err := n.st.DB().QueryRowContext(t.Context(), "SELECT in_seq FROM peer_links WHERE link_id = ?", link).Scan(&s)
	if err != nil {
		return 0
	}
	return s
}

// record is a valid next record from a to b on link, for the decision table.
func record(a, b node, link string, seq, origin int64, to string) Record {
	from := "sape"
	return Record{
		V: Version, Kind: kindMsg,
		Link:   Link{Store: a.id, ID: link, Seq: seq},
		Origin: Ident{Store: a.id, ID: origin}, DstStore: b.id,
		To: to, From: &from, Label: "sape", Hop: 1, SentAt: "2026-08-04T12:00:00.000Z", Body: "hello",
	}
}

// TestRelayInDecisionTable walks the two tables in the plan row by row. Every
// case feeds a store that has already imported seq 1 of link L, then the
// record under test, then a probe — a replay of seq 1, which answers
// "duplicate" only if the run went on — so each case pins the outcome, its
// effect on the link's sequence, and whether the run stopped.
func TestRelayInDecisionTable(t *testing.T) {
	const L = "link-L"
	type tc struct {
		name     string
		prep     func(t *testing.T, a, b node)
		peer     string
		raw      []byte // used instead of rec when set
		rec      func(a, b node) Record
		status   string
		reason   string
		seqAfter int64 // in_seq of L after the record
		cont     bool  // whether the probe was answered
		exit     int
	}
	next := func(mut func(*Record)) func(a, b node) Record {
		return func(a, b node) Record {
			r := record(a, b, L, 2, 2, "kipa")
			if mut != nil {
				mut(&r)
			}
			return r
		}
	}
	cases := []tc{
		// Placement.
		{name: "unregistered peer", peer: "nobody", rec: next(nil), status: StatusRefused, reason: "unknown peer", seqAfter: 1, exit: 2},
		{name: "not json", raw: []byte("not json\n"), status: StatusRefused, reason: "malformed", seqAfter: 1, exit: 2},
		{name: "wrong field type", raw: []byte(`{"v":"1","kind":"msg"}` + "\n"), status: StatusRefused, reason: "malformed", seqAfter: 1, exit: 2},
		{name: "kind missing", rec: next(func(r *Record) { r.Kind = "" }), status: StatusRefused, reason: "malformed", seqAfter: 1, exit: 2},
		{name: "version zero", rec: next(func(r *Record) { r.V = 0 }), status: StatusRefused, reason: "malformed", seqAfter: 1, exit: 2},
		{name: "version negative", rec: next(func(r *Record) { r.V = -1 }), status: StatusRefused, reason: "malformed", seqAfter: 1, exit: 2},
		{name: "version above max", rec: next(func(r *Record) { r.V = Version + 1 }), status: StatusRefused, reason: "unsupported version", seqAfter: 1, exit: 2},
		{name: "reserved kind", rec: next(func(r *Record) { r.Kind = "roster" }), status: StatusRefused, reason: "unsupported kind", seqAfter: 1, exit: 2},
		{name: "link store not enrolled", rec: next(func(r *Record) { r.Link.Store = strings.Repeat("e", 32) }), status: StatusRefused, reason: "wrong store", seqAfter: 1, exit: 2},
		{name: "dst store is not this one", rec: next(func(r *Record) { r.DstStore = strings.Repeat("e", 32) }), status: StatusRefused, reason: "wrong store", seqAfter: 1, exit: 2},
		{name: "empty link id", rec: next(func(r *Record) { r.Link.ID = "" }), status: StatusRefused, reason: "malformed link", seqAfter: 1, exit: 2},
		{name: "zero seq", rec: next(func(r *Record) { r.Link.Seq = 0 }), status: StatusRefused, reason: "malformed link", seqAfter: 1, exit: 2},
		{
			name: "known link under another peer name",
			prep: func(t *testing.T, a, b node) {
				if _, err := deliver.AddPeer(t.Context(), b.st, "mac2", a.id); err != nil {
					t.Fatalf("add mac2: %v", err)
				}
			},
			peer: "mac2", rec: next(nil), status: StatusRefused, reason: "link mismatch", seqAfter: 1, exit: 2,
		},
		{name: "unknown link not starting at 1", rec: next(func(r *Record) { r.Link.ID = "link-M" }), status: StatusRefused, reason: "unknown link", seqAfter: 1, exit: 2},

		// Already consumed.
		{name: "consumed, invalid origin", rec: next(func(r *Record) { r.Link.Seq = 1; r.Origin.Store = strings.Repeat("e", 32) }), status: StatusRejected, reason: "previously rejected", seqAfter: 1, cont: true},
		{name: "consumed, row exists", rec: next(func(r *Record) { r.Link.Seq = 1; r.Origin.ID = 1 }), status: StatusDuplicate, seqAfter: 1, cont: true},
		{name: "consumed, no row", rec: next(func(r *Record) { r.Link.Seq = 1; r.Origin.ID = 99 }), status: StatusRejected, reason: "previously rejected", seqAfter: 1, cont: true},

		// Ahead of the link.
		{name: "gap", rec: next(func(r *Record) { r.Link.Seq = 3 }), status: StatusDeferred, reason: "gap", seqAfter: 1},

		// The next record.
		{name: "malformed origin store", rec: next(func(r *Record) { r.Origin.Store = strings.Repeat("e", 32) }), status: StatusRejected, reason: "malformed origin", seqAfter: 2, cont: true},
		{name: "non-positive origin id", rec: next(func(r *Record) { r.Origin.ID = 0 }), status: StatusRejected, reason: "malformed origin", seqAfter: 2, cont: true},
		{name: "via present", rec: next(func(r *Record) { r.Via = json.RawMessage(`["x"]`) }), status: StatusRejected, reason: "forwarding unsupported", seqAfter: 2, cont: true},
		{name: "hop above max", rec: next(func(r *Record) { r.Hop = deliver.MaxHop + 1 }), status: StatusRejected, reason: "hop", seqAfter: 2, cont: true},
		{name: "negative hop", rec: next(func(r *Record) { r.Hop = -1 }), status: StatusRejected, reason: "hop", seqAfter: 2, cont: true},
		{name: "body too large", rec: next(func(r *Record) { r.Body = strings.Repeat("x", deliver.MaxRelayBody+1) }), status: StatusRejected, reason: "body too large", seqAfter: 2, cont: true},
		{name: "origin already imported", rec: next(func(r *Record) { r.Origin.ID = 1 }), status: StatusDuplicate, seqAfter: 2, cont: true},
		{
			name: "per-peer cap",
			prep: func(t *testing.T, a, b node) {
				for i := 0; i < maxInboundPeer; i++ {
					if _, err := b.st.DB().ExecContext(t.Context(), `
						INSERT INTO messages(alias, body, hop, origin, created_at, src_peer, src_store, src_id)
						VALUES ('kipa', 'x', 0, 'human', ?, 'mac', ?, ?)`, b.st.Now(), a.id, 1000+i); err != nil {
						t.Fatalf("seed import: %v", err)
					}
				}
			},
			rec: next(nil), status: StatusDeferred, reason: "rate", seqAfter: 1,
		},
		{name: "to empty", rec: next(func(r *Record) { r.To = "" }), status: StatusRejected, reason: "no such alias", seqAfter: 2, cont: true},
		{name: "to with @", rec: next(func(r *Record) { r.To = "kipa@vm" }), status: StatusRejected, reason: "no such alias", seqAfter: 2, cont: true},
		{name: "to unknown", rec: next(func(r *Record) { r.To = "ghost" }), status: StatusRejected, reason: "live names here: kipa", seqAfter: 2, cont: true},
		{name: "imported", rec: next(nil), status: StatusImported, seqAfter: 2, cont: true},

		// Overlaps: the earlier row wins.
		{name: "existing origin with foreign origin store", rec: next(func(r *Record) { r.Origin.ID = 1; r.Origin.Store = strings.Repeat("e", 32) }), status: StatusRejected, reason: "malformed origin", seqAfter: 2, cont: true},
		{name: "existing origin with via", rec: next(func(r *Record) { r.Origin.ID = 1; r.Via = json.RawMessage(`[]`) }), status: StatusRejected, reason: "forwarding unsupported", seqAfter: 2, cont: true},
		{
			name: "cap reached on a duplicate",
			prep: func(t *testing.T, a, b node) {
				for i := 0; i < maxInboundPeer; i++ {
					if _, err := b.st.DB().ExecContext(t.Context(), `
						INSERT INTO messages(alias, body, hop, origin, created_at, src_peer, src_store, src_id)
						VALUES ('kipa', 'x', 0, 'human', ?, 'mac', ?, ?)`, b.st.Now(), a.id, 1000+i); err != nil {
						t.Fatalf("seed import: %v", err)
					}
				}
			},
			rec: next(func(r *Record) { r.Origin.ID = 1 }), status: StatusDuplicate, seqAfter: 2, cont: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b := pair(t)
			session(t, b, "s-kipa", "kipa")
			first := encode(t, record(a, b, L, 1, 1, "kipa"))
			if rs, code := in(t, b, "mac", first); code != 0 || len(rs) != 1 || rs[0].Status != StatusImported {
				t.Fatalf("seq 1 of L did not import: %+v exit %d", rs, code)
			}
			if c.prep != nil {
				c.prep(t, a, b)
			}

			stream := c.raw
			if stream == nil {
				stream = encode(t, c.rec(a, b))
			}
			stream = append(append([]byte{}, stream...), first...) // the probe
			peer := c.peer
			if peer == "" {
				peer = "mac"
			}

			rs, code := in(t, b, peer, stream)
			if code != c.exit {
				t.Errorf("exit = %d, want %d (results %+v)", code, c.exit, rs)
			}
			if len(rs) == 0 {
				t.Fatalf("no result for the record under test")
			}
			if rs[0].Status != c.status || !strings.Contains(rs[0].Reason, c.reason) {
				t.Errorf("result = %s %q, want %s containing %q", rs[0].Status, rs[0].Reason, c.status, c.reason)
			}
			if got := inSeq(t, b, L); got != c.seqAfter {
				t.Errorf("in_seq of L = %d, want %d", got, c.seqAfter)
			}
			continued := len(rs) == 2
			if continued != c.cont {
				t.Errorf("run continued = %v, want %v (results %+v)", continued, c.cont, rs)
			}
			if continued && rs[1].Status != StatusDuplicate {
				t.Errorf("probe = %s, want duplicate", rs[1].Status)
			}
		})
	}
}

// TestRelayInIsIdempotent feeds the same export twice: the second run changes
// nothing and answers every record as a duplicate. The unique index on the
// original identity is checked directly too, since dedupe is decided before
// it is reached and it would otherwise go unexercised.
func TestRelayInIsIdempotent(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	for i := 0; i < 3; i++ {
		sendRemote(t, a, "s-sape", "kipa", fmt.Sprintf("message %d", i))
	}
	stream := out(t, a)

	rs, code := in(t, b, "mac", stream)
	if code != 0 || len(rs) != 3 {
		t.Fatalf("first run: %+v exit %d", rs, code)
	}
	for _, r := range rs {
		if r.Status != StatusImported {
			t.Errorf("first run: %s %q", r.Status, r.Reason)
		}
	}
	rs, code = in(t, b, "mac", stream)
	if code != 0 || len(rs) != 3 {
		t.Fatalf("second run: %+v exit %d", rs, code)
	}
	for _, r := range rs {
		if r.Status != StatusDuplicate {
			t.Errorf("second run: %s, want duplicate", r.Status)
		}
	}
	if n := count(t, b, "SELECT count(*) FROM messages WHERE src_peer = 'mac'"); n != 3 {
		t.Errorf("%d rows imported, want 3", n)
	}

	if _, err := b.st.DB().ExecContext(t.Context(), `
		INSERT INTO messages(alias, body, hop, origin, created_at, src_peer, src_store, src_id)
		VALUES ('kipa', 'copy', 0, 'human', 0, 'mac', ?, 1)`, a.id); err == nil {
		t.Error("a second row with the same original identity was accepted; messages_src is missing")
	}
}

// TestRelayInDefersOnGap drops a line from the middle of a stream. The record
// after the gap is deferred and the run stops there, so nothing is taken out
// of order; the full stream afterwards fills the gap and continues.
func TestRelayInDefersOnGap(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	for i := 0; i < 3; i++ {
		sendRemote(t, a, "s-sape", "kipa", fmt.Sprintf("message %d", i))
	}
	ls := lines(out(t, a))
	holed := append(append([]byte{}, ls[0]...), ls[2]...)

	rs, code := in(t, b, "mac", holed)
	if code != 0 {
		t.Errorf("exit = %d, want 0: a gap is transient", code)
	}
	if len(rs) != 2 || rs[0].Status != StatusImported || rs[1].Status != StatusDeferred || !strings.Contains(rs[1].Reason, "gap") {
		t.Fatalf("results = %+v, want imported then deferred(gap)", rs)
	}
	if n := count(t, b, "SELECT count(*) FROM messages WHERE src_peer = 'mac'"); n != 1 {
		t.Errorf("%d rows imported across a gap, want 1", n)
	}

	rs, _ = in(t, b, "mac", out(t, a))
	want := []string{StatusDuplicate, StatusImported, StatusImported}
	for i, r := range rs {
		if r.Status != want[i] {
			t.Errorf("refill result %d = %s, want %s", i, r.Status, want[i])
		}
	}
}

// TestRelayRejectBouncesOnceAfterLostResult loses the result line of a
// rejection. The resend is consumed already, and must still be answered as a
// rejection — otherwise the sender would ack it as delivered and never hear
// — and applying results any number of times writes exactly one bounce.
func TestRelayRejectBouncesOnceAfterLostResult(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "s-sape", "nobody", "are you there?")

	first, _ := in(t, b, "mac", out(t, a))
	if len(first) != 1 || first[0].Status != StatusRejected {
		t.Fatalf("first run = %+v, want rejected", first)
	}
	// Its result is lost; the record is still pending and goes again.
	replay, _ := in(t, b, "mac", out(t, a))
	if len(replay) != 1 || replay[0].Status != StatusRejected || replay[0].Reason != "previously rejected" {
		t.Fatalf("replay = %+v, want rejected(previously rejected)", replay)
	}

	results := encode(t, replay[0])
	for i := 0; i < 3; i++ {
		if code := ack(t, a, results); code != 0 {
			t.Fatalf("ack %d exit %d", i, code)
		}
	}
	if code := ack(t, a, encode(t, first[0])); code != 0 {
		t.Fatalf("late ack of the lost result: exit %d", code)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE origin = 'caused' AND sender_label = 'pager' AND alias = 'sape'"); n != 1 {
		t.Errorf("%d bounces written, want exactly 1", n)
	}
	var result string
	if err := a.st.DB().QueryRowContext(t.Context(),
		"SELECT relay_result FROM messages WHERE id = 1").Scan(&result); err != nil {
		t.Fatalf("read relay_result: %v", err)
	}
	if !strings.HasPrefix(result, "rejected") {
		t.Errorf("relay_result = %q, want rejected…", result)
	}
}

// TestRelayBounceSkipsUnaddressableSender: a send with no session, and one
// from a session that holds no name, have nobody to tell. No bounce row is
// written — one addressed to a stranger's name would be worse — and the
// outcome stays on the outbound row.
func TestRelayBounceSkipsUnaddressableSender(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-nameless", "")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "", "nobody", "from a person")
	sendRemote(t, a, "s-nameless", "nobody", "from an unnamed session")

	rs, _ := in(t, b, "mac", out(t, a))
	var results []byte
	for _, r := range rs {
		results = append(results, encode(t, r)...)
	}
	if code := ack(t, a, results); code != 0 {
		t.Fatalf("ack exit %d", code)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE origin = 'caused'"); n != 0 {
		t.Errorf("%d bounces written for senders with no name", n)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE dst_peer = 'vm' AND relay_result LIKE 'rejected%'"); n != 2 {
		t.Errorf("%d outbound rows record the rejection, want 2", n)
	}
}

// TestRelayInRefusesForeignDstStore: a stream meant for another store — a
// transport that landed on the wrong database — is refused before anything is
// imported or any link is touched.
func TestRelayInRefusesForeignDstStore(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "s-sape", "kipa", "for b")
	stream := out(t, a)

	other := newNode(t)
	if _, err := deliver.AddPeer(t.Context(), other.st, "mac", a.id); err != nil {
		t.Fatalf("add peer: %v", err)
	}
	session(t, other, "s-kipa", "kipa")
	rs, code := in(t, other, "mac", stream)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if len(rs) != 1 || rs[0].Status != StatusRefused || !strings.Contains(rs[0].Reason, "wrong store") {
		t.Errorf("results = %+v, want refused(wrong store)", rs)
	}
	if n := count(t, other, "SELECT count(*) FROM messages"); n != 0 {
		t.Errorf("%d rows imported into the wrong store", n)
	}
	if n := count(t, other, "SELECT count(*) FROM peer_links"); n != 0 {
		t.Errorf("%d links created in the wrong store", n)
	}
}

// TestRelayAckRejectsWholeBatchOnStoreMismatch: one result from a store that
// is not the one enrolled refuses the batch before any line is applied.
func TestRelayAckRejectsWholeBatchOnStoreMismatch(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "s-sape", "kipa", "one")
	sendRemote(t, a, "s-sape", "kipa", "two")
	rs, _ := in(t, b, "mac", out(t, a))

	forged := rs[1]
	forged.Store = strings.Repeat("e", 32)
	batch := append(encode(t, rs[0]), encode(t, forged)...)
	if code := ack(t, a, batch); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE dst_peer = 'vm' AND relayed_at IS NOT NULL"); n != 0 {
		t.Errorf("%d rows acked from a refused batch", n)
	}

	if code := ack(t, a, append(encode(t, rs[0]), encode(t, rs[1])...)); code != 0 {
		t.Fatalf("honest batch: exit %d", code)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE dst_peer = 'vm' AND relayed_at IS NOT NULL"); n != 2 {
		t.Errorf("%d rows acked from an honest batch, want 2", n)
	}
}

// TestRelayInDefersAtPeerCap: past the per-peer cap a record is deferred, not
// lost, with a retry hint; once the window has moved it imports.
func TestRelayInDefersAtPeerCap(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	for i := 0; i < maxInboundPeer; i++ {
		if _, err := b.st.DB().ExecContext(t.Context(), `
			INSERT INTO messages(alias, body, hop, origin, created_at, src_peer, src_store, src_id)
			VALUES ('kipa', 'x', 0, 'human', ?, 'mac', ?, ?)`, b.st.Now(), a.id, 1000+i); err != nil {
			t.Fatalf("seed import: %v", err)
		}
	}
	sendRemote(t, a, "s-sape", "kipa", "over the cap")
	stream := out(t, a)

	rs, code := in(t, b, "mac", stream)
	if code != 0 || len(rs) != 1 || rs[0].Status != StatusDeferred || !strings.Contains(rs[0].Reason, "rate") {
		t.Fatalf("results = %+v exit %d, want deferred(rate) exit 0", rs, code)
	}
	if rs[0].RetryAfterMS <= 0 {
		t.Errorf("retry_after_ms = %d, want a positive hint", rs[0].RetryAfterMS)
	}

	b.fake.Advance(inboundWindow + time.Second)
	rs, _ = in(t, b, "mac", stream)
	if len(rs) != 1 || rs[0].Status != StatusImported {
		t.Errorf("after the window: %+v, want imported", rs)
	}
}

// TestConcurrentRelayInImportsOnce runs the same stream through several
// relay in runs at once. Each record is decided under the write lock, so
// every message is imported exactly once whatever the interleaving.
func TestConcurrentRelayInImportsOnce(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	for i := 0; i < 5; i++ {
		sendRemote(t, a, "s-sape", "kipa", fmt.Sprintf("message %d", i))
	}
	stream := out(t, a)

	const runs = 8
	var wg sync.WaitGroup
	results := make([][]Result, runs)
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf bytes.Buffer
			if err := In(t.Context(), b.st, "mac", bytes.NewReader(stream), &buf, io.Discard); err != nil {
				t.Errorf("run %d: %v", i, err)
			}
			results[i] = parseResults(t, buf.Bytes())
		}()
	}
	wg.Wait()

	imported := map[int64]int{}
	for _, rs := range results {
		for _, r := range rs {
			if r.Status == StatusImported {
				imported[r.Link.Seq]++
			}
		}
	}
	for seq := int64(1); seq <= 5; seq++ {
		if imported[seq] != 1 {
			t.Errorf("seq %d imported %d times, want exactly once", seq, imported[seq])
		}
	}
	if n := count(t, b, "SELECT count(*) FROM messages WHERE src_peer = 'mac'"); n != 5 {
		t.Errorf("%d rows imported, want 5", n)
	}
}

// TestPeerRemoveForceCancelsAndBounces: forcing a peer out closes its pending
// mail as cancelled, tells each sender — saying it may still arrive — and
// never sends it again, even once a peer of the same name is added back.
func TestPeerRemoveForceCancelsAndBounces(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	sendRemote(t, a, "s-sape", "kipa", "one")
	sendRemote(t, a, "s-sape", "kipa", "two")

	if err := deliver.RemovePeer(t.Context(), a.st, "vm"); !errors.Is(err, deliver.ErrPeerHasPendingMail) {
		t.Fatalf("plain remove: err = %v, want ErrPeerHasPendingMail", err)
	}
	if err := ForceRemovePeer(t.Context(), a.st, "vm", io.Discard); err != nil {
		t.Fatalf("force remove: %v", err)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE dst_peer = 'vm' AND relay_result LIKE 'cancelled%' AND relayed_at IS NOT NULL"); n != 2 {
		t.Errorf("%d rows cancelled, want 2", n)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE origin = 'caused' AND alias = 'sape' AND body LIKE '%may still arrive%'"); n != 2 {
		t.Errorf("%d cancellation bounces, want 2", n)
	}
	if n := count(t, a, "SELECT count(*) FROM peers"); n != 0 {
		t.Errorf("peer still registered after force remove")
	}

	if _, err := deliver.AddPeer(t.Context(), a.st, "vm", b.id); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if got := out(t, a); len(got) != 0 {
		t.Errorf("relay out after re-add re-sent cancelled mail:\n%s", got)
	}
}

// TestPeerReAddKeepsInboundLinks: removing and re-adding a peer on the
// receiving side keeps its inbound link state, so the sender's next record
// continues the link instead of being refused as unknown, and an old one is
// still a duplicate.
func TestPeerReAddKeepsInboundLinks(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "s-sape", "kipa", "one")
	sendRemote(t, a, "s-sape", "kipa", "two")
	first := out(t, a)
	if rs, _ := in(t, b, "mac", first); len(rs) != 2 {
		t.Fatalf("first import: %+v", rs)
	}

	if err := deliver.RemovePeer(t.Context(), b.st, "mac"); err != nil {
		t.Fatalf("remove on b: %v", err)
	}
	if _, err := deliver.AddPeer(t.Context(), b.st, "mac", a.id); err != nil {
		t.Fatalf("re-add on b: %v", err)
	}

	sendRemote(t, a, "s-sape", "kipa", "three")
	rs, code := in(t, b, "mac", out(t, a)) // seq 1 and 2 are still pending on a, then 3
	if code != 0 || len(rs) != 3 {
		t.Fatalf("after re-add: %+v exit %d", rs, code)
	}
	want := []string{StatusDuplicate, StatusDuplicate, StatusImported}
	for i, r := range rs {
		if r.Status != want[i] {
			t.Errorf("result %d = %s %q, want %s", i, r.Status, r.Reason, want[i])
		}
	}
}

// TestRelayOutReplaysInOrderAfterLostAck: until a final result is acked,
// relay out re-emits exactly the same records in the same order; the
// receiver answers them as duplicates, and once acked nothing is left.
func TestRelayOutReplaysInOrderAfterLostAck(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	for i := 0; i < 3; i++ {
		sendRemote(t, a, "s-sape", "kipa", fmt.Sprintf("message %d", i))
	}

	first := out(t, a)
	if rs, _ := in(t, b, "mac", first); len(rs) != 3 {
		t.Fatalf("first import: %+v", rs)
	}
	second := out(t, a) // the results were lost: nothing was acked
	if !bytes.Equal(first, second) {
		t.Fatalf("replay differs from the first export:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	rs, _ := in(t, b, "mac", second)
	var results []byte
	for i, r := range rs {
		if r.Status != StatusDuplicate || r.Link.Seq != int64(i+1) {
			t.Errorf("replay result %d = %s seq %d, want duplicate seq %d", i, r.Status, r.Link.Seq, i+1)
		}
		results = append(results, encode(t, r)...)
	}
	if code := ack(t, a, results); code != 0 {
		t.Fatalf("ack exit %d", code)
	}
	if rest := out(t, a); len(rest) != 0 {
		t.Errorf("relay out after the ack still emits:\n%s", rest)
	}
}

// TestRelayBounceIsDeliveredOnce follows a bounce to where it matters: the
// sender's next hook delivery. However many times the rejection is acked, the
// sender reads one message saying what happened, from pager, as quoted data.
func TestRelayBounceIsDeliveredOnce(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "s-sape", "ghost", "anyone?")

	rs, _ := in(t, b, "mac", out(t, a))
	results := encode(t, rs[0])
	for i := 0; i < 3; i++ {
		if code := ack(t, a, results); code != 0 {
			t.Fatalf("ack %d: exit %d", i, code)
		}
	}

	pending, _, err := deliver.Candidates(t.Context(), a.st, "s-sape", deliver.DefaultLimits())
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("the sender has %d messages waiting, want the one bounce: %+v", len(pending), pending)
	}
	rendered := deliver.Render(deliver.Batch{Messages: pending})
	if n := strings.Count(rendered, "was not delivered"); n != 1 {
		t.Errorf("bounce appears %d times in the delivery:\n%s", n, rendered)
	}
	for _, want := range []string{"from pager", "> your message #1 to ghost@vm was not delivered: no such alias ghost"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("delivery lacks %q:\n%s", want, rendered)
		}
	}
}

// pendingLinks returns (dst_link, dst_seq) of a's mail still waiting for vm.
func pendingLinks(t *testing.T, a node) [][2]any {
	t.Helper()
	rows, err := a.st.DB().QueryContext(t.Context(),
		"SELECT dst_link, dst_seq FROM messages WHERE dst_peer = 'vm' AND relayed_at IS NULL ORDER BY id")
	if err != nil {
		t.Fatalf("read pending: %v", err)
	}
	defer rows.Close()
	var out [][2]any
	for rows.Next() {
		var link string
		var seq int64
		if err := rows.Scan(&link, &seq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, [2]any{link, seq})
	}
	return out
}

// TestRebindRenumbersAndIgnoresStaleResults: a rebind moves pending mail to a
// new link numbered from 1, and a result still in flight for the old link —
// here, the receiver's answers to the first export, acked only after the
// rebind — matches nothing. Rebinding to a different store refuses that
// store's predecessor's results outright.
func TestRebindRenumbersAndIgnoresStaleResults(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	for i := 0; i < 3; i++ {
		sendRemote(t, a, "s-sape", "kipa", fmt.Sprintf("message %d", i))
	}
	oldLinks := pendingLinks(t, a)
	stale, _ := in(t, b, "mac", out(t, a))

	p, err := deliver.RebindPeer(t.Context(), a.st, "vm", b.id)
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	now := pendingLinks(t, a)
	for i, l := range now {
		if l[0] != p.LinkID || l[1] != int64(i+1) {
			t.Errorf("pending row %d = %v, want (%s, %d)", i, l, p.LinkID, i+1)
		}
		if l[0] == oldLinks[i][0] {
			t.Errorf("pending row %d still on the old link", i)
		}
	}
	if p.OutSeq != 3 {
		t.Errorf("out_seq after rebind = %d, want 3", p.OutSeq)
	}

	var batch []byte
	for _, r := range stale {
		batch = append(batch, encode(t, r)...)
	}
	if code := ack(t, a, batch); code != 0 {
		t.Fatalf("ack of stale results: exit %d", code)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE dst_peer = 'vm' AND relayed_at IS NOT NULL"); n != 0 {
		t.Errorf("%d rows acked by results for a link that no longer carries them", n)
	}

	c := newNode(t)
	if _, err := deliver.RebindPeer(t.Context(), a.st, "vm", c.id); err != nil {
		t.Fatalf("rebind to another store: %v", err)
	}
	if code := ack(t, a, batch); code != 2 {
		t.Errorf("results from the store vm used to be: exit %d, want 2", code)
	}
}

// TestRebindReplayAdvancesLink: after a same-store rebind the receiver sees a
// new link whose first records are messages it already imported. They are
// answered as duplicates and still advance the link, so the record after them
// imports instead of waiting on a gap — and the original-identity index is
// never violated.
func TestRebindReplayAdvancesLink(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "s-sape", "kipa", "one")
	sendRemote(t, a, "s-sape", "kipa", "two")
	if rs, _ := in(t, b, "mac", out(t, a)); len(rs) != 2 {
		t.Fatalf("first import: %+v", rs)
	} // results lost: both stay pending on a

	p, err := deliver.RebindPeer(t.Context(), a.st, "vm", b.id)
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	sendRemote(t, a, "s-sape", "kipa", "three")

	rs, code := in(t, b, "mac", out(t, a))
	if code != 0 || len(rs) != 3 {
		t.Fatalf("after rebind: %+v exit %d", rs, code)
	}
	want := []string{StatusDuplicate, StatusDuplicate, StatusImported}
	for i, r := range rs {
		if r.Status != want[i] || r.Link.ID != p.LinkID {
			t.Errorf("result %d = %s on %s, want %s on the new link", i, r.Status, r.Link.ID, want[i])
		}
	}
	if got := inSeq(t, b, p.LinkID); got != 3 {
		t.Errorf("in_seq of the new link = %d, want 3", got)
	}
	if n := count(t, b, "SELECT count(*) FROM messages WHERE src_peer = 'mac'"); n != 3 {
		t.Errorf("%d rows imported, want 3", n)
	}
}

// TestNewIDStartsFreshLinks: a store that takes a new id — what a restore
// from backup calls for — starts new links under it, and its peer refuses it
// until the new id is enrolled there. Mail still pending at that moment cannot
// move to the new id: version 1 sends a message under the id of the link that
// carries it, so a message the peer already imported would look new and arrive
// twice. So a plain new-id is refused while mail is pending, and --force
// cancels it first, telling each sender it may still arrive.
func TestNewIDStartsFreshLinks(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "s-sape", "kipa", "one")
	if rs, _ := in(t, b, "mac", out(t, a)); len(rs) != 1 || rs[0].Status != StatusImported {
		t.Fatalf("first import: %+v", rs)
	} // its result is lost: "one" stays pending on a, though b has it

	if _, err := deliver.RotateStoreID(t.Context(), a.st); !errors.Is(err, deliver.ErrPeerHasPendingMail) {
		t.Fatalf("new-id with pending mail: err = %v, want ErrPeerHasPendingMail", err)
	}
	if got, _ := a.st.ID(t.Context()); got != a.id {
		t.Fatalf("a refused new-id changed the store id")
	}

	id, err := ForceRotateStoreID(t.Context(), a.st, io.Discard)
	if err != nil {
		t.Fatalf("new-id --force: %v", err)
	}
	if got, _ := a.st.ID(t.Context()); got != id || id == a.id {
		t.Fatalf("store id after rotate = %s (returned %s, was %s)", got, id, a.id)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE origin = 'caused' AND alias = 'sape' AND body LIKE '%may still arrive%'"); n != 1 {
		t.Errorf("%d cancellation bounces, want 1", n)
	}

	sendRemote(t, a, "s-sape", "kipa", "two")
	stream := out(t, a)
	if got := len(lines(stream)); got != 1 {
		t.Fatalf("relay out after new-id emits %d records, want only the new one:\n%s", got, stream)
	}
	rs, code := in(t, b, "mac", stream)
	if code != 2 || len(rs) != 1 || !strings.Contains(rs[0].Reason, "wrong store") {
		t.Fatalf("before re-enrolment: %+v exit %d, want refused(wrong store)", rs, code)
	}

	if _, err := deliver.RebindPeer(t.Context(), b.st, "mac", id); err != nil {
		t.Fatalf("b enrols the new id: %v", err)
	}
	rs, code = in(t, b, "mac", stream)
	if code != 0 || len(rs) != 1 || rs[0].Status != StatusImported {
		t.Errorf("after re-enrolment: %+v exit %d, want imported", rs, code)
	}
	if n := count(t, b, "SELECT count(*) FROM messages WHERE src_peer = 'mac' AND body = 'one'"); n != 1 {
		t.Errorf("%q arrived %d times, want once", "one", n)
	}
}
