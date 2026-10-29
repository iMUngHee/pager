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

// Historical values used to prove ordinary imports no longer wait for an hourly quota.
const maxInboundPeer = 60

func TestExtendedEventMetadataAndLongChainsCrossRelay(t *testing.T) {
	a, b := pair(t)
	session(t, a, "sender", "sape")
	session(t, b, "receiver", "kipa")
	meta := deliver.Metadata{Task: "gpu-job", Kind: "release", Activity: "idle", Progress: "process exited", EventID: "gpu-lease-7"}
	sent, err := deliver.Send(t.Context(), a.st, deliver.SendRequest{Alias: "kipa@vm", Peer: "vm", Sender: "sender", Body: "GPU released", Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.st.DB().Exec("UPDATE messages SET hop=45 WHERE id=?", sent.ID); err != nil {
		t.Fatal(err)
	}
	stream := out(t, a)
	var wire Record
	if err := json.Unmarshal(bytes.TrimSpace(stream), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.V != 2 || wire.Hop != 45 || wire.Metadata != meta {
		t.Fatalf("metadata lost on wire: %+v", wire)
	}
	rs, code := in(t, b, "mac", stream)
	if code != 0 || len(rs) != 1 || rs[0].Status != StatusImported {
		t.Fatalf("import=%+v exit=%d", rs, code)
	}
	var got deliver.Metadata
	var hop int
	if err := b.st.DB().QueryRow("SELECT task,event_kind,activity,progress,event_id,hop FROM messages WHERE src_peer='mac'").Scan(&got.Task, &got.Kind, &got.Activity, &got.Progress, &got.EventID, &hop); err != nil {
		t.Fatal(err)
	}
	if got != meta || hop != 45 {
		t.Fatalf("stored metadata=%+v hop=%d", got, hop)
	}
	if code := ack(t, a, encode(t, rs[0])); code != 0 {
		t.Fatalf("ack exit=%d", code)
	}
	if len(out(t, a)) != 0 {
		t.Fatal("acknowledged event remained in relay out")
	}
}

func TestBasicRelayKeepsV1AndRejectsSemanticSmuggling(t *testing.T) {
	a, b := pair(t)
	session(t, a, "sender", "sape")
	session(t, b, "receiver", "kipa")
	sendRemote(t, a, "sender", "kipa", "ordinary mail")
	var wire Record
	if err := json.Unmarshal(bytes.TrimSpace(out(t, a)), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.V != 1 {
		t.Fatalf("basic mail unnecessarily requires v%d", wire.V)
	}
	wire.Metadata.Kind = "release"
	rs, _ := in(t, b, "mac", encode(t, wire))
	if len(rs) != 1 || rs[0].Status != StatusRejected || !strings.Contains(rs[0].Reason, "v2") {
		t.Fatalf("v1 semantic extension accepted: %+v", rs)
	}
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
		noResult bool // refused before any line is read: no result at all
	}
	var reboundTo string // set by the rebind row's prep
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
		{name: "unregistered peer", peer: "nobody", rec: next(nil), noResult: true, seqAfter: 1, exit: 2},
		{name: "not json", raw: []byte("not json\n"), status: StatusRefused, reason: "malformed", seqAfter: 1, exit: 2},
		{name: "wrong field type", raw: []byte(`{"v":"1","kind":"msg"}` + "\n"), status: StatusRefused, reason: "malformed", seqAfter: 1, exit: 2},
		{name: "kind missing", rec: next(func(r *Record) { r.Kind = "" }), status: StatusRefused, reason: "malformed", seqAfter: 1, exit: 2},
		{name: "version zero", rec: next(func(r *Record) { r.V = 0 }), status: StatusRefused, reason: "malformed", seqAfter: 1, exit: 2},
		{name: "version negative", rec: next(func(r *Record) { r.V = -1 }), status: StatusRefused, reason: "malformed", seqAfter: 1, exit: 2},
		{name: "version above max", rec: next(func(r *Record) { r.V = Version + 1 }), status: StatusRefused, reason: "unsupported version", seqAfter: 1, exit: 2},
		{name: "reserved kind", rec: next(func(r *Record) { r.Kind = "roster" }), status: StatusRefused, reason: "unsupported kind", seqAfter: 1, exit: 2},
		{name: "kind with a control character", rec: next(func(r *Record) { r.Kind = "\x1b[31mred" }), status: StatusRefused, reason: `unsupported kind \u001b[31mred`, seqAfter: 1, exit: 2},
		{name: "link store not enrolled", rec: next(func(r *Record) { r.Link.Store = strings.Repeat("e", 32) }), status: StatusRefused, reason: "wrong store: the record comes from store " + strings.Repeat("e", 32), seqAfter: 1, exit: 2},
		{name: "dst store is not this one", rec: next(func(r *Record) { r.DstStore = strings.Repeat("e", 32) }), status: StatusRefused, reason: "wrong store: the record is for store " + strings.Repeat("e", 32), seqAfter: 1, exit: 2},
		// The reason quotes the incoming id, so it is cleaned; and with both
		// stores wrong, the record's addressee is what the reason names.
		{name: "both stores wrong", rec: next(func(r *Record) { r.Link.Store, r.DstStore = strings.Repeat("e", 32), "\x1b[2J"+strings.Repeat("f", 32) }), status: StatusRefused, reason: `wrong store: the record is for store \u001b[2J` + strings.Repeat("f", 32) + ", and this is ", seqAfter: 1, exit: 2},
		{name: "link store with a control character", rec: next(func(r *Record) { r.Link.Store = "\x1b[2J" + strings.Repeat("e", 32) }), status: StatusRefused, reason: `wrong store: the record comes from store \u001b[2J` + strings.Repeat("e", 32) + ", but peer", seqAfter: 1, exit: 2},
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
		{name: "unknown link id with a control character", rec: next(func(r *Record) { r.Link.ID = "\x1b[2Jlink-M" }), status: StatusRefused, reason: `unknown link \u001b[2Jlink-M starting at 2`, seqAfter: 1, exit: 2},
		{
			name: "known link id with a control character, other pairing",
			prep: func(t *testing.T, a, b node) {
				if _, err := b.st.DB().ExecContext(t.Context(), `
					INSERT INTO peer_links(link_id, peer, src_store, dst_store, in_seq) VALUES (?, 'mac', ?, ?, 1)`,
					"\x1b[2Jlink-X", strings.Repeat("e", 32), b.id); err != nil {
					t.Fatalf("seed link: %v", err)
				}
			},
			rec:    next(func(r *Record) { r.Link.ID = "\x1b[2Jlink-X" }),
			status: StatusRefused, reason: `link mismatch: link \u001b[2Jlink-X belongs`, seqAfter: 1, exit: 2,
		},
		{
			name: "known link from a store mac was rebound away from",
			prep: func(t *testing.T, a, b node) {
				other := newNode(t)
				if _, err := deliver.RebindPeer(t.Context(), b.st, "mac", other.id); err != nil {
					t.Fatalf("rebind: %v", err)
				}
				reboundTo = other.id
			},
			rec: func(a, b node) Record {
				r := record(a, b, L, 2, 2, "kipa")
				r.Link.Store, r.Origin.Store = reboundTo, reboundTo // L itself is still known as a's
				return r
			},
			status: StatusRefused, reason: "link mismatch", seqAfter: 1, exit: 2,
		},

		// Already consumed.
		{name: "consumed, invalid origin", rec: next(func(r *Record) { r.Link.Seq = 1; r.Origin.Store = strings.Repeat("e", 32) }), status: StatusRejected, reason: "previously rejected", seqAfter: 1, cont: true},
		{name: "consumed, row exists", rec: next(func(r *Record) { r.Link.Seq = 1; r.Origin.ID = 1 }), status: StatusDuplicate, seqAfter: 1, cont: true},
		{name: "consumed, no row", rec: next(func(r *Record) { r.Link.Seq = 1; r.Origin.ID = 99 }), status: StatusRejected, reason: "previously rejected", seqAfter: 1, cont: true},
		{
			name: "consumed, foreign origin that exists here",
			prep: func(t *testing.T, a, b node) {
				if _, err := b.st.DB().ExecContext(t.Context(), `
					INSERT INTO messages(alias, body, hop, origin, created_at, src_peer, src_store, src_id)
					VALUES ('kipa', 'x', 0, 'human', ?, 'lab', ?, 1)`, b.st.Now(), strings.Repeat("e", 32)); err != nil {
					t.Fatalf("seed foreign row: %v", err)
				}
			},
			rec: next(func(r *Record) { r.Link.Seq = 1; r.Origin.ID = 1; r.Origin.Store = strings.Repeat("e", 32) }), status: StatusRejected, reason: "previously rejected", seqAfter: 1, cont: true,
		},

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
			name: "past historical per-peer cap",
			prep: func(t *testing.T, a, b node) {
				for i := 0; i < maxInboundPeer; i++ {
					if _, err := b.st.DB().ExecContext(t.Context(), `
						INSERT INTO messages(alias, body, hop, origin, created_at, src_peer, src_store, src_id)
						VALUES ('kipa', 'x', 0, 'human', ?, 'mac', ?, ?)`, b.st.Now(), a.id, 1000+i); err != nil {
						t.Fatalf("seed import: %v", err)
					}
				}
			},
			rec: next(nil), status: StatusImported, seqAfter: 2, cont: true,
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
			if c.noResult {
				if len(rs) != 0 {
					t.Errorf("results = %+v, want none: the run is refused before reading", rs)
				}
				if got := inSeq(t, b, L); got != c.seqAfter {
					t.Errorf("in_seq of L = %d, want %d", got, c.seqAfter)
				}
				return
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
	forged.Store = "\x1b[2J" + strings.Repeat("e", 32)
	batch := append(encode(t, rs[0]), encode(t, forged)...)
	err := Ack(t.Context(), a.st, "vm", bytes.NewReader(batch), io.Discard)
	if code := exitCode(t, err); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	// The refusal reaches a terminal on stderr, so the id it quotes is cleaned.
	if msg := err.Error(); strings.Contains(msg, "\x1b") || !strings.Contains(msg, `from store \u001b[2J`) {
		t.Errorf("refusal %q does not escape the store id it quotes", msg)
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

// TestRejectionNamesEachLiveSessionOnce: the names a "no such alias" rejection
// offers are one per live session, the one it is best known by, so a session
// holding an automatic name and a chosen one is not listed twice, and a
// session that has gone quiet is not offered at all. A name holding '@' cannot
// be addressed from another store, so it is never offered, and a session whose
// newest name is one is offered by the next.
func TestRejectionNamesEachLiveSessionOnce(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-gone", "aaa")
	b.fake.Advance(store.DefaultStale + time.Minute)

	// s-1 was recorded first and sorts first, but its name sorts last.
	session(t, b, "s-1", "zulu")
	session(t, b, "s-2", "kipa")
	if ok, err := deliver.SetAlias(t.Context(), b.st, "maba", "s-2"); err != nil || !ok {
		t.Fatalf("second name: %v %v", ok, err)
	}
	// Names from before '@' was refused: s-2's newest, and the only one s-3
	// holds, which leaves s-3 live with nothing to offer.
	session(t, b, "s-3", "")
	for _, l := range []struct{ alias, session string }{{"maba@old", "s-2"}, {"only@at", "s-3"}} {
		if _, err := b.st.DB().ExecContext(t.Context(), `
			INSERT INTO aliases(alias, session_id, root, tool, updated_at) VALUES (?, ?, '/tmp/ws', 'claude', ?)`,
			l.alias, l.session, b.st.Now()+1000); err != nil {
			t.Fatalf("legacy name %s: %v", l.alias, err)
		}
	}
	// Live means within the stale window, not "at this very instant".
	b.fake.Advance(time.Minute)

	sendRemote(t, a, "s-sape", "ghost", "hello")
	rs, code := in(t, b, "mac", out(t, a))
	want := "no such alias ghost; live names here: maba, zulu"
	if code != 0 || len(rs) != 1 || rs[0].Status != StatusRejected || rs[0].Reason != want {
		t.Fatalf("results = %+v exit %d, want one rejected with reason %q", rs, code, want)
	}
}

// TestRelayInContinuesPastFormerPeerQuota imports immediately and deduplicates retries.
func TestRelayInContinuesPastFormerPeerQuota(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	for i := 0; i < maxInboundPeer+5; i++ {
		sendRemote(t, a, "s-sape", "kipa", fmt.Sprintf("ordinary exchange %d", i))
	}
	stream := out(t, a)
	rs, code := in(t, b, "mac", stream)
	if code != 0 || len(rs) != maxInboundPeer+5 {
		t.Fatalf("results=%d, exit=%d", len(rs), code)
	}
	for _, r := range rs {
		if r.Status != StatusImported {
			t.Fatalf("unexpected quota: %+v", r)
		}
	}
	rs, code = in(t, b, "mac", stream)
	for _, r := range rs {
		if r.Status != StatusDuplicate {
			t.Fatalf("retry=%+v, exit=%d", r, code)
		}
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
	if !strings.Contains(rs[0].Reason, "from store "+id) {
		t.Errorf("reason %q does not name the new id %s to rebind to", rs[0].Reason, id)
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

// TestRelayInRefusesUnknownPeerBeforeReading: a --peer that is not registered
// is refused before any line is read — even from an empty stream — and a
// refusal of a line says which store is answering.
func TestRelayInRefusesUnknownPeerBeforeReading(t *testing.T) {
	_, b := pair(t)
	if _, code := in(t, b, "nobody", nil); code != 2 {
		t.Errorf("empty stream for an unregistered peer: exit %d, want 2", code)
	}
	rs, code := in(t, b, "mac", []byte("not json\n"))
	if code != 2 || len(rs) != 1 || rs[0].Status != StatusRefused || rs[0].Store != b.id {
		t.Errorf("malformed line: %+v exit %d, want one refusal naming store %s", rs, code, b.id)
	}
}

// TestRelayInSkipsBlankLines: a blank line carries no record and is not a
// reason to stop, so a transport that adds one does not stall the link.
func TestRelayInSkipsBlankLines(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "s-sape", "kipa", "one")
	sendRemote(t, a, "s-sape", "kipa", "two")
	ls := lines(out(t, a))
	stream := append(append(append([]byte("\n"), ls[0]...), "\r\n  \n"...), ls[1]...)
	rs, code := in(t, b, "mac", stream)
	if code != 0 || len(rs) != 2 || rs[0].Status != StatusImported || rs[1].Status != StatusImported {
		t.Errorf("results = %+v exit %d, want two imports", rs, code)
	}
}

// TestDedupeKeysOnOriginStore: two stores may each send their message #1. A
// duplicate is the same original identity — store and id — never the id alone.
func TestDedupeKeysOnOriginStore(t *testing.T) {
	a, b := pair(t)
	c := newNode(t)
	if _, err := deliver.AddPeer(t.Context(), c.st, "vm", b.id); err != nil {
		t.Fatalf("c adds vm: %v", err)
	}
	if _, err := deliver.AddPeer(t.Context(), b.st, "lab", c.id); err != nil {
		t.Fatalf("b adds lab: %v", err)
	}
	session(t, a, "s-sape", "sape")
	session(t, c, "s-lima", "lima")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "s-sape", "kipa", "from a")
	sendRemote(t, c, "s-lima", "kipa", "from c")

	if rs, _ := in(t, b, "mac", out(t, a)); len(rs) != 1 || rs[0].Status != StatusImported {
		t.Fatalf("from a: %+v", rs)
	}
	if rs, _ := in(t, b, "lab", out(t, c)); len(rs) != 1 || rs[0].Status != StatusImported {
		t.Errorf("from c, the same message id in another store: %+v, want imported", rs)
	}
}

// TestAckLeavesNonFinalResultsPending: a deferred result is not an outcome;
// the message stays pending and goes out again.
func TestAckLeavesNonFinalResultsPending(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "s-sape", "kipa", "one")
	rs, _ := in(t, b, "mac", out(t, a))
	deferred := rs[0]
	deferred.Status, deferred.Reason = StatusDeferred, "gap"
	if code := ack(t, a, encode(t, deferred)); code != 0 {
		t.Fatalf("ack: exit %d", code)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE dst_peer = 'vm' AND relayed_at IS NULL"); n != 1 {
		t.Errorf("%d rows still pending after a deferred result, want 1", n)
	}
	if got := len(lines(out(t, a))); got != 1 {
		t.Errorf("relay out emits %d records after a deferred result, want 1", got)
	}
}

// TestAckBouncesOnlyRejections: imported and duplicate results close their
// message without telling the sender anything.
func TestAckBouncesOnlyRejections(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "s-sape", "kipa", "one")
	stream := out(t, a)
	first, _ := in(t, b, "mac", stream)
	again, _ := in(t, b, "mac", stream)
	if code := ack(t, a, append(encode(t, first[0]), encode(t, again[0])...)); code != 0 {
		t.Fatalf("ack: exit %d", code)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE origin = 'caused'"); n != 0 {
		t.Errorf("%d bounces written for delivered mail", n)
	}
}

// TestRelayInWakesWhomItDelivered runs with wake enabled against an empty
// HOME, so a poke attempt is visible as the "not poked" note: relay in tries
// to wake each recipient it imported for, once, after committing.
func TestRelayInWakesWhomItDelivered(t *testing.T) {
	t.Setenv("PAGER_WAKE", "on")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	sendRemote(t, a, "s-sape", "kipa", "one")
	sendRemote(t, a, "s-sape", "kipa", "two")

	var diag bytes.Buffer
	if err := In(t.Context(), b.st, "mac", bytes.NewReader(out(t, a)), io.Discard, &diag); err != nil {
		t.Fatalf("relay in: %v", err)
	}
	if n := strings.Count(diag.String(), "kipa was not poked"); n != 1 {
		t.Errorf("wake attempts reported %d times, want once for the one recipient:\n%s", n, diag.String())
	}
}

// TestRelayInKeepsTheHop: the imported row carries the hop the message had in
// the store that wrote it — the loop guard across stores rests on it.
func TestRelayInKeepsTheHop(t *testing.T) {
	a, b := pair(t)
	session(t, b, "s-kipa", "kipa")
	r := record(a, b, "link-L", 1, 7, "kipa")
	r.Hop = 2
	if rs, _ := in(t, b, "mac", encode(t, r)); len(rs) != 1 || rs[0].Status != StatusImported {
		t.Fatalf("import: %+v", rs)
	}
	if n := count(t, b, "SELECT count(*) FROM messages WHERE src_id = 7 AND hop = 2"); n != 1 {
		t.Error("the imported row does not carry hop 2")
	}
}

// TestInboundCapIsPerPeer: one peer at its cap does not hold back another.
func TestInboundCapIsPerPeer(t *testing.T) {
	a, b := pair(t)
	session(t, b, "s-kipa", "kipa")
	for i := 0; i < maxInboundPeer; i++ {
		if _, err := b.st.DB().ExecContext(t.Context(), `
			INSERT INTO messages(alias, body, hop, origin, created_at, src_peer, src_store, src_id)
			VALUES ('kipa', 'x', 0, 'human', ?, 'lab', ?, ?)`, b.st.Now(), strings.Repeat("e", 32), i+1); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if rs, _ := in(t, b, "mac", encode(t, record(a, b, "link-L", 1, 1, "kipa"))); len(rs) != 1 || rs[0].Status != StatusImported {
		t.Errorf("mac while lab is at its cap: %+v, want imported", rs)
	}
}

// TestRebindRenumbersFromOneAfterPartialAck: after part of a link has been
// acknowledged, a rebind still numbers what is left from 1 — otherwise the
// receiver would refuse the new link as starting mid-way.
func TestRebindRenumbersFromOneAfterPartialAck(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	for i := 0; i < 3; i++ {
		sendRemote(t, a, "s-sape", "kipa", fmt.Sprintf("message %d", i))
	}
	rs, _ := in(t, b, "mac", out(t, a))
	if code := ack(t, a, encode(t, rs[0])); code != 0 {
		t.Fatalf("ack: exit %d", code)
	}
	p, err := deliver.RebindPeer(t.Context(), a.st, "vm", b.id)
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	got := pendingLinks(t, a)
	if len(got) != 2 || got[0] != [2]any{p.LinkID, int64(1)} || got[1] != [2]any{p.LinkID, int64(2)} {
		t.Fatalf("pending after rebind = %v, want seq 1 and 2 on the new link", got)
	}
	rs, code := in(t, b, "mac", out(t, a))
	if code != 0 || len(rs) != 2 || rs[0].Status != StatusDuplicate || rs[1].Status != StatusDuplicate {
		t.Errorf("after rebind: %+v exit %d, want two duplicates on a link the receiver accepts", rs, code)
	}
}

// TestNewIDSucceedsWithoutPendingMail: with nothing waiting to leave — local
// mail and finished relay history do not count — a plain new-id goes ahead.
func TestNewIDSucceedsWithoutPendingMail(t *testing.T) {
	a, b := pair(t)
	session(t, a, "s-sape", "sape")
	session(t, b, "s-kipa", "kipa")
	if _, err := deliver.Send(t.Context(), a.st, deliver.SendRequest{Alias: "sape", Body: "local", Human: true}); err != nil {
		t.Fatalf("local send: %v", err)
	}
	sendRemote(t, a, "s-sape", "kipa", "one")
	rs, _ := in(t, b, "mac", out(t, a))
	if code := ack(t, a, encode(t, rs[0])); code != 0 {
		t.Fatalf("ack: exit %d", code)
	}
	id, err := deliver.RotateStoreID(t.Context(), a.st)
	if err != nil {
		t.Fatalf("new-id with nothing pending: %v", err)
	}
	if got, _ := a.st.ID(t.Context()); got != id || id == a.id {
		t.Errorf("store id = %s after rotate (returned %s, was %s)", got, id, a.id)
	}
}

// TestForceNewIDCancelsEveryPendingRowOnEveryPeer: --force closes all of it,
// across peers, before the id changes, so nothing is left to go out under the
// new id.
func TestForceNewIDCancelsEveryPendingRowOnEveryPeer(t *testing.T) {
	a, _ := pair(t)
	c := newNode(t)
	if _, err := deliver.AddPeer(t.Context(), a.st, "lab", c.id); err != nil {
		t.Fatalf("add lab: %v", err)
	}
	session(t, a, "s-sape", "sape")
	for i := 0; i < 2; i++ {
		sendRemote(t, a, "s-sape", "kipa", fmt.Sprintf("to vm %d", i))
		label, _ := deliver.SenderLabel(t.Context(), a.st, "s-sape")
		if _, err := deliver.Send(t.Context(), a.st, deliver.SendRequest{
			Alias: "kipa@lab", Peer: "lab", Body: fmt.Sprintf("to lab %d", i), Sender: "s-sape", Label: label,
		}); err != nil {
			t.Fatalf("send to lab: %v", err)
		}
	}
	if _, err := ForceRotateStoreID(t.Context(), a.st, io.Discard); err != nil {
		t.Fatalf("new-id --force: %v", err)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE dst_peer IS NOT NULL AND relayed_at IS NULL"); n != 0 {
		t.Errorf("%d rows still pending after new-id --force", n)
	}
	if n := count(t, a, "SELECT count(*) FROM messages WHERE origin = 'caused' AND alias = 'sape'"); n != 4 {
		t.Errorf("%d bounces, want 4", n)
	}
	for _, peer := range []string{"vm", "lab"} {
		var buf bytes.Buffer
		if err := Out(t.Context(), a.st, peer, DefaultLimit, &buf); err != nil || buf.Len() != 0 {
			t.Errorf("relay out --peer %s after new-id --force: %q, %v", peer, buf.String(), err)
		}
	}
}

// TestRelayOutDropsAnOverlongReplyName: a reply name the receiver would drop is
// not sent at all, so a long alias cannot swell the record.
func TestRelayOutDropsAnOverlongReplyName(t *testing.T) {
	a, _ := pair(t)
	long := strings.Repeat("n", maxFrom+1)
	session(t, a, "s-long", long)
	sendRemote(t, a, "s-long", "kipa", "hi")
	var rec Record
	if err := json.Unmarshal(lines(out(t, a))[0], &rec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.From != nil {
		t.Errorf("from = %q, want it left out", *rec.From)
	}
}

// TestRelayOutDropsAnOverlongTool: the sender's host name is bounded like
// every other field — a value longer than the receiver keeps is not sent, so
// it cannot swell a record past the line bound — while an ordinary one, and
// one exactly at the bound, still go out.
func TestRelayOutDropsAnOverlongTool(t *testing.T) {
	a, _ := pair(t)
	for _, tool := range []string{"claude", strings.Repeat("t", maxTool)} {
		if err := a.st.RecordSession(t.Context(), store.SessionRecord{ID: "s-" + tool, Tool: tool, Root: "/tmp/ws"}); err != nil {
			t.Fatalf("record: %v", err)
		}
		if ok, err := deliver.SetAlias(t.Context(), a.st, "n"+fmt.Sprint(len(tool)), "s-"+tool); err != nil || !ok {
			t.Fatalf("alias: %v %v", ok, err)
		}
		sendRemote(t, a, "s-"+tool, "kipa", "hi")
	}
	for i, want := range []string{"claude", strings.Repeat("t", maxTool)} {
		var rec Record
		if err := json.Unmarshal(lines(out(t, a))[i], &rec); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if rec.Tool != want {
			t.Errorf("record %d tool = %q, want %q", i, rec.Tool, want)
		}
	}

	if err := a.st.RecordSession(t.Context(), store.SessionRecord{ID: "s-odd", Tool: strings.Repeat("t", maxTool+1), Root: "/tmp/ws"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if ok, err := deliver.SetAlias(t.Context(), a.st, "odd", "s-odd"); err != nil || !ok {
		t.Fatalf("alias: %v %v", ok, err)
	}
	sendRemote(t, a, "s-odd", "kipa", "hi")
	var rec Record
	if err := json.Unmarshal(lines(out(t, a))[2], &rec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.Tool != "" {
		t.Errorf("tool = %q, want it left out", rec.Tool)
	}
}

// TestRelayInRefusesAnOverlongLineNamingItself: the refusal of a line past the
// bound says which store is answering, like every other refusal.
func TestRelayInRefusesAnOverlongLineNamingItself(t *testing.T) {
	_, b := pair(t)
	rs, code := in(t, b, "mac", []byte(strings.Repeat("x", MaxLine+1)+"\n"))
	if code != 2 || len(rs) != 1 || rs[0].Status != StatusRefused || rs[0].Store != b.id {
		t.Errorf("overlong line: %+v exit %d, want one refusal naming store %s", rs, code, b.id)
	}
}
