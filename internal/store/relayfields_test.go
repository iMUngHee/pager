package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/iMUngHee/pager/internal/clock"
)

// TestPruneRespectsRelayState: outbound mail is kept until it has a final
// result however old it is — deleting it would lose it with nobody told —
// while finished outbound mail ages out like delivered mail, and the existing
// exclusions still protect a row something points at.
func TestPruneRespectsRelayState(t *testing.T) {
	s, fake := newStore(t)
	t.Setenv("PAGER_ARCHIVE", "off")
	ctx := t.Context()
	now := s.Now()

	insert := func(query string, args ...any) int64 {
		t.Helper()
		res, err := s.db.ExecContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	pending := insert(`INSERT INTO messages(alias, body, hop, origin, created_at, dst_peer, dst_link, dst_seq)
		VALUES ('kipa@vm', 'waiting to leave', 0, 'human', ?, 'vm', 'l', 1)`, now)
	finished := insert(`INSERT INTO messages(alias, body, hop, origin, created_at, dst_peer, dst_link, dst_seq, relayed_at, relay_result)
		VALUES ('kipa@vm', 'gone out', 0, 'human', ?, 'vm', 'l', 2, ?, 'imported')`, now, now)
	bounced := insert(`INSERT INTO messages(alias, body, hop, origin, created_at, dst_peer, dst_link, dst_seq, relayed_at, relay_result)
		VALUES ('ghost@vm', 'rejected', 0, 'human', ?, 'vm', 'l', 3, ?, 'rejected: no such alias')`, now, now)
	bounce := insert(`INSERT INTO messages(alias, body, hop, origin, cause_id, created_at)
		VALUES ('sape', 'your message bounced', 0, 'caused', ?, ?)`, bounced, now)
	imported := insert(`INSERT INTO messages(alias, body, hop, origin, created_at, delivered_at, src_peer, src_store, src_id)
		VALUES ('kipa', 'came in', 1, 'human', ?, ?, 'mac', 'x', 1)`, now, now)

	fake.Advance(DefaultRetention + time.Hour)
	if _, err := s.PruneNow(ctx, DefaultRetention, false); err != nil {
		t.Fatalf("PruneNow: %v", err)
	}

	alive := func(id int64) bool {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE id = ?", id).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n == 1
	}
	for _, tc := range []struct {
		name string
		id   int64
		want bool
	}{
		{"unacked outbound", pending, true},
		{"acked outbound", finished, false},
		{"acked outbound a bounce points at", bounced, true},
		{"undelivered bounce", bounce, true},
		{"delivered import", imported, false},
	} {
		if got := alive(tc.id); got != tc.want {
			t.Errorf("%s (#%d): kept = %v, want %v", tc.name, tc.id, got, tc.want)
		}
	}
}

// TestExportCarriesRelayFields: every exported record carries the relay
// fields — null on local mail, whether the row was written before the relay
// migration or after — and their values on relayed mail.
func TestExportCarriesRelayFields(t *testing.T) {
	relayKeys := []string{"src_peer", "src_store", "src_id", "src_from", "dst_peer", "dst_link", "dst_seq", "relayed_at", "relay_result"}

	// A row written by a store that predates relaying, then migrated.
	path := openAtVersion(t, 3)
	if _, err := Open(t.Context(), path, clock.NewFake(testBase)); err != nil {
		t.Fatalf("seed open: %v", err)
	}
	old, err := Open(t.Context(), path, clock.NewFake(testBase))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = old.Close() })
	if _, err := old.db.ExecContext(t.Context(),
		"INSERT INTO messages(alias, body, hop, origin, created_at) VALUES ('kipa', 'old', 0, 'human', 1)"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	s, _ := newStore(t)
	now := s.Now()
	if _, err := s.db.ExecContext(t.Context(), `
		INSERT INTO messages(alias, body, hop, origin, created_at) VALUES ('kipa', 'local', 0, 'human', ?)`, now); err != nil {
		t.Fatalf("insert local: %v", err)
	}
	if _, err := s.db.ExecContext(t.Context(), `
		INSERT INTO messages(alias, body, hop, origin, created_at, src_peer, src_store, src_id, src_from)
		VALUES ('kipa', 'in', 1, 'human', ?, 'mac', 'abc', 41, 'sape')`, now); err != nil {
		t.Fatalf("insert imported: %v", err)
	}
	if _, err := s.db.ExecContext(t.Context(), `
		INSERT INTO messages(alias, body, hop, origin, created_at, dst_peer, dst_link, dst_seq, relayed_at, relay_result)
		VALUES ('kipa@vm', 'out', 0, 'human', ?, 'vm', 'lnk', 7, ?, 'imported')`, now, now); err != nil {
		t.Fatalf("insert outbound: %v", err)
	}

	decode := func(line string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode %s: %v", line, err)
		}
		return m
	}
	for _, line := range append(exportLines(t, old), exportLines(t, s)[0]) {
		m := decode(line)
		for _, k := range relayKeys {
			v, ok := m[k]
			if !ok {
				t.Errorf("local record lacks %q: %s", k, line)
			} else if v != nil {
				t.Errorf("local record has %s = %v, want null", k, v)
			}
		}
	}

	lines := exportLines(t, s)
	in, out := decode(lines[1]), decode(lines[2])
	if in["src_peer"] != "mac" || in["src_store"] != "abc" || in["src_id"] != float64(41) || in["src_from"] != "sape" {
		t.Errorf("imported record = %v", in)
	}
	if out["dst_peer"] != "vm" || out["dst_link"] != "lnk" || out["dst_seq"] != float64(7) || out["relay_result"] != "imported" {
		t.Errorf("outbound record = %v", out)
	}
	if ts, _ := out["relayed_at"].(string); ts != archiveStamp(now) {
		t.Errorf("relayed_at = %v, want %s", out["relayed_at"], archiveStamp(now))
	}
}
