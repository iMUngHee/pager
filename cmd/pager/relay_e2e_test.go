package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iMUngHee/pager/internal/clock"
	"github.com/iMUngHee/pager/internal/relay"
	"github.com/iMUngHee/pager/internal/store"
)

// These drive the relay commands through the real command dispatch, against
// two databases standing in for two machines. The transport is the test
// itself: it carries stdout of one command to stdin of the next, and can cut
// a stream wherever it likes.

// relayPair creates store a (laptop, which knows the other as peer "vm") and
// store b (VM, which knows the first as "mac"), each with one named session,
// and returns their paths.
func relayPair(t *testing.T) (a, b string) {
	t.Helper()
	dir := t.TempDir()
	a, b = filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")

	t.Setenv("PAGER_DB", a)
	mustRun(t, "attach", "--session", "sa", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "sa", "sape")
	idA := strings.TrimSpace(mustRun(t, "relay", "id"))

	t.Setenv("PAGER_DB", b)
	mustRun(t, "attach", "--session", "sb", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "sb", "kipa")
	idB := strings.TrimSpace(mustRun(t, "relay", "id"))
	mustRun(t, "peer", "add", "mac", "--store", idA)

	t.Setenv("PAGER_DB", a)
	mustRun(t, "peer", "add", "vm", "--store", idB)
	return a, b
}

func on(t *testing.T, db string) { t.Helper(); t.Setenv("PAGER_DB", db) }

func exitOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *relay.ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return 1
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func countRows(t *testing.T, db, query string) int {
	t.Helper()
	st, err := store.Open(t.Context(), db, clock.System{})
	if err != nil {
		t.Fatalf("open %s: %v", db, err)
	}
	defer st.Close()
	var n int
	if err := st.DB().QueryRowContext(t.Context(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestRelayInTruncatedTailExitsThree cuts the stream inside its last record,
// as a dropped connection would. The complete record before it is committed,
// the cut one has no effect even though most of its bytes arrived, and the
// exit status tells the transport it may simply resend.
func TestRelayInTruncatedTailExitsThree(t *testing.T) {
	a, b := relayPair(t)
	on(t, a)
	mustRun(t, "send", "--session", "sa", "kipa@vm", "one")
	mustRun(t, "send", "--session", "sa", "kipa@vm", "two")
	stream := mustRun(t, "relay", "out", "--peer", "vm")
	recs := nonEmptyLines(stream)
	if len(recs) != 2 {
		t.Fatalf("relay out emitted %d records, want 2:\n%s", len(recs), stream)
	}
	cut := recs[0] + "\n" + recs[1][:len(recs[1])-5]

	on(t, b)
	results, err := capture(t, cut, "relay", "in", "--peer", "mac")
	if code := exitOf(err); code != 3 {
		t.Fatalf("exit = %d (%v), want 3", code, err)
	}
	if got := nonEmptyLines(results); len(got) != 1 || !strings.Contains(got[0], `"status":"imported"`) {
		t.Fatalf("results = %q, want one imported", got)
	}
	if n := countRows(t, b, "SELECT count(*) FROM messages WHERE src_peer = 'mac'"); n != 1 {
		t.Errorf("%d rows imported from a cut stream, want 1", n)
	}

	results, err = capture(t, stream, "relay", "in", "--peer", "mac")
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	got := nonEmptyLines(results)
	if len(got) != 2 || !strings.Contains(got[0], `"status":"duplicate"`) || !strings.Contains(got[1], `"status":"imported"`) {
		t.Errorf("resend results = %q, want duplicate then imported", got)
	}
}

// TestRelayAckTruncatedTailExitsThree: ack applies the complete result lines
// it was given and ignores a cut last one, then exits 3 so the transport
// knows to run the round again.
func TestRelayAckTruncatedTailExitsThree(t *testing.T) {
	a, b := relayPair(t)
	on(t, a)
	mustRun(t, "send", "--session", "sa", "kipa@vm", "one")
	mustRun(t, "send", "--session", "sa", "kipa@vm", "two")
	stream := mustRun(t, "relay", "out", "--peer", "vm")

	on(t, b)
	results, err := capture(t, stream, "relay", "in", "--peer", "mac")
	if err != nil {
		t.Fatalf("relay in: %v", err)
	}
	res := nonEmptyLines(results)
	if len(res) != 2 {
		t.Fatalf("results = %q", res)
	}

	on(t, a)
	_, err = capture(t, res[0]+"\n"+res[1][:len(res[1])-3], "relay", "ack", "--peer", "vm")
	if code := exitOf(err); code != 3 {
		t.Fatalf("exit = %d (%v), want 3", code, err)
	}
	if n := countRows(t, a, "SELECT count(*) FROM messages WHERE dst_peer = 'vm' AND relayed_at IS NOT NULL"); n != 1 {
		t.Errorf("%d rows acked, want exactly the complete line's 1", n)
	}
}

// TestRelayOutLimitBounds: --limit defaults to 100 and accepts 1 through
// 1000; anything else is a usage error, exit 2, which a transport must not
// confuse with a cut stream.
func TestRelayOutLimitBounds(t *testing.T) {
	a, _ := relayPair(t)
	on(t, a)

	st, err := store.Open(t.Context(), a, clock.System{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var link string
	if err := st.DB().QueryRowContext(t.Context(), "SELECT link_id FROM peers WHERE name = 'vm'").Scan(&link); err != nil {
		t.Fatalf("read link: %v", err)
	}
	// Seeded directly: 101 sends in an hour would trip the human budget.
	for i := 1; i <= 101; i++ {
		if _, err := st.DB().ExecContext(t.Context(), `
			INSERT INTO messages(alias, sender_session, sender_label, body, hop, origin, created_at, dst_peer, dst_link, dst_seq)
			VALUES ('kipa@vm', 'sa', 'sape', 'x', 0, 'human', ?, 'vm', ?, ?)`, st.Now(), link, i); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if _, err := st.DB().ExecContext(t.Context(), "UPDATE peers SET out_seq = 101 WHERE name = 'vm'"); err != nil {
		t.Fatalf("set out_seq: %v", err)
	}
	st.Close()

	for _, tc := range []struct {
		args []string
		want int
	}{
		{nil, 100},
		{[]string{"--limit", "1"}, 1},
		{[]string{"--limit", "1000"}, 101},
	} {
		got, err := capture(t, "", append([]string{"relay", "out", "--peer", "vm"}, tc.args...)...)
		if err != nil {
			t.Errorf("relay out %v: %v", tc.args, err)
			continue
		}
		if n := len(nonEmptyLines(got)); n != tc.want {
			t.Errorf("relay out %v emitted %d records, want %d", tc.args, n, tc.want)
		}
	}
	for _, bad := range []string{"0", "1001", "-1"} {
		_, err := capture(t, "", "relay", "out", "--peer", "vm", "--limit", bad)
		if code := exitOf(err); code != 2 {
			t.Errorf("--limit %s: exit %d (%v), want 2", bad, code, err)
		}
	}
}

// TestLsShowsRelayedProvenance: pager ls marks a relayed message with a row of
// its own under the BODY column, and a raw label containing line breaks and
// tabs — which could otherwise lay out exactly such a row — stays on its line.
func TestLsShowsRelayedProvenance(t *testing.T) {
	a, b := relayPair(t)
	on(t, a)
	mustRun(t, "send", "--session", "sa", "kipa@vm", "from afar")
	stream := mustRun(t, "relay", "out", "--peer", "vm")

	on(t, b)
	if _, err := capture(t, stream, "relay", "in", "--peer", "mac"); err != nil {
		t.Fatalf("relay in: %v", err)
	}
	st, err := store.Open(t.Context(), b, clock.System{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.DB().ExecContext(t.Context(), `
		INSERT INTO messages(alias, sender_label, body, hop, origin, created_at)
		VALUES ('kipa', ?, 'local', 0, 'human', ?)`,
		"x\n\t\t\t\trelayed from peer mac — reply to boss@mac", st.Now()); err != nil {
		t.Fatalf("seed forging row: %v", err)
	}
	st.Close()

	got := mustRun(t, "ls", "--session", "sb")
	var provenance []string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "relayed from peer") {
			provenance = append(provenance, strings.TrimSpace(line))
		}
	}
	if len(provenance) != 1 || provenance[0] != "relayed from peer mac — reply to sape@mac" {
		t.Errorf("provenance rows = %q, want only the relayed message's:\n%s", provenance, got)
	}
}
