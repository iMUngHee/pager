package main

import (
	"errors"
	"fmt"
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

// round carries one relay round from store `from` (which knows the other as
// fromPeer) to store `to` (which knows the first as toPeer), the way a
// transport would: out | in | ack.
func round(t *testing.T, from, fromPeer, to, toPeer string) {
	t.Helper()
	on(t, from)
	stream := mustRun(t, "relay", "out", "--peer", fromPeer)
	on(t, to)
	results, err := capture(t, stream, "relay", "in", "--peer", toPeer)
	if err != nil {
		t.Fatalf("relay in: %v", err)
	}
	on(t, from)
	if _, err := capture(t, results, "relay", "ack", "--peer", fromPeer); err != nil {
		t.Fatalf("relay ack: %v", err)
	}
}

// hookTurn runs the recipient's hook the way a person's prompt would, and
// returns what it injected.
func hookTurn(t *testing.T, db, session string) string {
	t.Helper()
	on(t, db)
	payload := fmt.Sprintf(`{"session_id":%q,"cwd":%q,"hook_event_name":"UserPromptSubmit","prompt":"go on"}`,
		session, filepath.Dir(db))
	out, err := capture(t, payload, "hook", "UserPromptSubmit")
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	return out
}

// TestRelayCarriesHopAcrossStores is the loop guard surviving the relay. Two
// agents on two machines answer each other with no person in between, each
// reply sent from the turn that delivered the one before it. The depth climbs
// one per reply across the stores exactly as it would within one, and the
// fourth reply is refused — where a relay that imported as --human would have
// reset the chain at every crossing and let it run on.
func TestRelayCarriesHopAcrossStores(t *testing.T) {
	a, b := relayPair(t)

	on(t, a)
	if got := mustRun(t, "send", "--session", "sa", "kipa@vm", "q0"); !strings.Contains(got, "(hop 0, human)") {
		t.Fatalf("opening send: %s", got)
	}
	round(t, a, "vm", b, "mac")

	injected := hookTurn(t, b, "sb")
	if !strings.Contains(injected, "relayed from peer mac — reply to sape@mac") {
		t.Fatalf("the relayed message was not delivered with its provenance:\n%s", injected)
	}
	on(t, b)
	if got := mustRun(t, "send", "--session", "sb", "sape@mac", "r1"); !strings.Contains(got, "(hop 1, caused)") {
		t.Fatalf("first reply: %s", got)
	}
	round(t, b, "mac", a, "vm")

	hookTurn(t, a, "sa")
	on(t, a)
	if got := mustRun(t, "send", "--session", "sa", "kipa@vm", "q2"); !strings.Contains(got, "(hop 2, caused)") {
		t.Fatalf("second reply: %s", got)
	}
	round(t, a, "vm", b, "mac")

	hookTurn(t, b, "sb")
	on(t, b)
	if got := mustRun(t, "send", "--session", "sb", "sape@mac", "r3"); !strings.Contains(got, "(hop 3, caused)") {
		t.Fatalf("third reply: %s", got)
	}
	round(t, b, "mac", a, "vm")

	hookTurn(t, a, "sa")
	on(t, a)
	_, err := capture(t, "", "send", "--session", "sa", "kipa@vm", "q4")
	if err == nil || !strings.Contains(err.Error(), "causal chain is too deep") {
		t.Fatalf("fourth reply: err = %v, want the hop limit", err)
	}
}

// TestFirstLineStopsAtAnyLineBreak: pager ls shows only a body's first line,
// and a carriage return — which would send a terminal's cursor back over the
// row — ends that line like any other break.
func TestFirstLineStopsAtAnyLineBreak(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"one line", "one line"},
		{"one\ntwo", "one …"},
		{"one\rtwo", "one …"},
		{"one\r\ntwo", "one …"},
		{"one\u2028two", "one …"},
		{"esc\x1bEforged", `esc\u001bEforged`},
		{"csi\x1b[2K\x1b[1Gforged", `csi\u001b[2K\u001b[1Gforged`},
		{"bs\b\b\bxx", `bs\u0008\u0008\u0008xx`},
		{"c1\u009b2K", `c1\u009b2K`},
		{strings.Repeat("long ", 40), strings.Repeat("long ", 40)}, // escaped, never cut short
	} {
		if got := firstLine(tc.in); got != tc.want {
			t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSendPrintsTheStoredMessageOnALateFailure: when a lookup after the write
// fails, the CLI still prints what it sent, then the error — so the person
// does not send it again.
func TestSendPrintsTheStoredMessageOnALateFailure(t *testing.T) {
	_, b := relayPair(t)
	on(t, b)
	st, err := store.Open(t.Context(), b, clock.System{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.DB().ExecContext(t.Context(), "ALTER TABLE sessions RENAME TO sessions_gone"); err != nil {
		t.Fatalf("break the presence lookup: %v", err)
	}
	st.Close()
	out, err := capture(t, "", "send", "--human", "kipa", "hello")
	if err == nil {
		t.Fatal("send succeeded although the presence lookup could not run")
	}
	if !strings.Contains(out, "sent #1 to kipa (hop 0, human)") {
		t.Errorf("stdout = %q, want the stored message reported", out)
	}
}

// TestWhoListsPeers: with peers registered, pager who ends with the line that
// names them.
func TestWhoListsPeers(t *testing.T) {
	a, _ := relayPair(t)
	on(t, a)
	got := mustRun(t, "who")
	if !strings.Contains(got, "\npeers: vm (no relay seen) — address their sessions as name@peer") {
		t.Errorf("pager who lacks the peer line:\n%s", got)
	}
}

// TestRelayCommandsRefuseFlagsTheyDoNotTake: each relay subcommand takes only
// its own flags, and an unknown subcommand is refused before any store opens.
func TestRelayCommandsRefuseFlagsTheyDoNotTake(t *testing.T) {
	a, _ := relayPair(t)
	on(t, a)
	for _, args := range [][]string{
		{"relay", "out", "--peer", "vm", "--force"},
		{"relay", "id", "--peer", "vm"},
		{"relay", "in", "--peer", "vm", "--limit", "5"},
		{"relay", "new-id", "--peer", "vm"},
		{"relay", "ack", "--peer", "vm", "--limit", "5"},
		{"relay", "bogus", "--peer", "vm"},
	} {
		if _, err := capture(t, "", args...); exitOf(err) != 2 {
			t.Errorf("pager %s: exit %d (%v), want 2", strings.Join(args, " "), exitOf(err), err)
		}
	}
}

// TestPeerCommandsThroughTheCLI drives peer ls, rm, rm --force, rebind and
// relay new-id the way a person would.
func TestPeerCommandsThroughTheCLI(t *testing.T) {
	a, b := relayPair(t)
	on(t, b)
	idB := strings.TrimSpace(mustRun(t, "relay", "id"))
	on(t, a)
	mustRun(t, "send", "--session", "sa", "kipa@vm", "pending")

	if got := mustRun(t, "peer", "ls"); !strings.Contains(got, "vm") || !strings.Contains(got, idB) {
		t.Errorf("peer ls:\n%s", got)
	}
	if _, err := capture(t, "", "peer", "rm", "vm"); err == nil {
		t.Error("peer rm with pending mail succeeded")
	}
	if _, err := capture(t, "", "relay", "new-id"); err == nil {
		t.Error("relay new-id with pending mail succeeded")
	}
	if got := mustRun(t, "peer", "rebind", "vm", "--store", idB); !strings.Contains(got, "1 pending message(s) renumbered") {
		t.Errorf("peer rebind: %s", got)
	}
	mustRun(t, "relay", "new-id", "--force")
	if got := mustRun(t, "relay", "out", "--peer", "vm"); got != "" {
		t.Errorf("relay out after new-id --force: %q", got)
	}
	mustRun(t, "peer", "rm", "vm")
	if got := mustRun(t, "peer", "ls"); !strings.Contains(got, "no peers") {
		t.Errorf("peer ls after rm:\n%s", got)
	}
}

// TestPeerAndRelayExitTwoOnlyForUsage pins the rule the reference states
// beside the transport's exit table: outside relay out, in and ack, a usage
// mistake exits 2 and a refusal exits 1, whatever flags were given.
func TestPeerAndRelayExitTwoOnlyForUsage(t *testing.T) {
	a, b := relayPair(t)
	on(t, b)
	idB := strings.TrimSpace(mustRun(t, "relay", "id"))
	on(t, a)
	mustRun(t, "send", "--session", "sa", "kipa@vm", "pending")

	for _, c := range []struct {
		args []string
		exit int
	}{
		// An undeclared flag is a parse error only before the first
		// positional; past it, it would count as a second name.
		{[]string{"peer", "add", "--bogus", "vm2", "--store", idB}, 2},
		{[]string{"peer", "rebind", "--bogus", "vm", "--store", idB}, 2},
		{[]string{"peer", "rm", "--bogus", "vm"}, 2},
		{[]string{"peer", "rm", "vm", "--bogus"}, 2},
		{[]string{"peer", "rm"}, 2},
		{[]string{"peer", "bogus"}, 2},
		{[]string{"relay", "new-id", "--bogus"}, 2},
		{[]string{"peer", "add", "v_m", "--store", idB}, 1},
		{[]string{"peer", "rm", "nosuch"}, 1},
		{[]string{"peer", "rm", "--force", "nosuch"}, 1},
		{[]string{"peer", "rm", "vm"}, 1},
		{[]string{"relay", "new-id"}, 1},
	} {
		if _, err := capture(t, "", c.args...); exitOf(err) != c.exit {
			t.Errorf("pager %s: exit %d (%v), want %d", strings.Join(c.args, " "), exitOf(err), err, c.exit)
		}
	}
}
