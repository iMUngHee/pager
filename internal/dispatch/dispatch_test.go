package dispatch

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/iMUngHee/pager/internal/clock"
	"github.com/iMUngHee/pager/internal/deliver"
	"github.com/iMUngHee/pager/internal/store"
)

const workspace = "/tmp/workspace"

// otherStore is a well-formed store id that is not the test store's own.
const otherStore = "0123456789abcdef0123456789abcdef"

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), ".pager", "msg.db"),
		clock.NewFake(time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// named records a session holding alias. pmRef may be "".
func named(t *testing.T, st *store.Store, session, alias, pmRef string) {
	t.Helper()
	if err := st.RecordSession(t.Context(), store.SessionRecord{
		ID: session, Tool: "claude", Root: workspace, PMRef: pmRef,
	}); err != nil {
		t.Fatalf("record %s: %v", session, err)
	}
	if ok, err := deliver.SetAlias(t.Context(), st, alias, session); err != nil || !ok {
		t.Fatalf("alias %s -> %s: %v, %v", alias, session, ok, err)
	}
}

func alive(int, int64) (bool, bool) { return true, true }
func gone(int, int64) (bool, bool)  { return false, true }

// TestLocalSendWordingUnchanged pins what a local send says, in the exact words
// the CLI and the MCP server printed before they shared this path. The literals
// are the point: a reword here is a change to two user-facing surfaces at once.
func TestLocalSendWordingUnchanged(t *testing.T) {
	st := newStore(t)
	named(t, st, "s-kipa", "kipa", "")
	named(t, st, "s-sape", "sape", "")

	res, err := Send(t.Context(), st, Request{Target: "kipa", Body: "hi", Human: true, Probe: alive})
	if err != nil {
		t.Fatalf("human send: %v", err)
	}
	if got := res.Head(); got != "sent #1 to kipa (hop 0, human)" {
		t.Errorf("head = %q", got)
	}
	if len(res.Lines) != 0 {
		t.Errorf("a held recipient with wake off adds %q, want nothing", res.Lines)
	}

	res, err = Send(t.Context(), st, Request{Target: "kipa", Body: "again", Sender: "s-sape", Probe: gone})
	if err != nil {
		t.Fatalf("attributed send: %v", err)
	}
	if got := res.Head(); got != "sent #2 to kipa (hop 0, human)" {
		t.Errorf("head = %q", got)
	}
	want := []string{"note: kipa's session is gone — the message waits, but nothing will read it " +
		"until a session in that workspace claims the name; pager who lists who can answer now"}
	if !slices.Equal(res.Lines, want) {
		t.Errorf("stranded recipient lines = %q, want %q", res.Lines, want)
	}

	var label string
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT sender_label FROM messages WHERE id = 2").Scan(&label); err != nil {
		t.Fatalf("read label: %v", err)
	}
	if label != "sape" {
		t.Errorf("default label = %q, want the sender's alias", label)
	}
}

// TestAddressWithoutAtKeepsTiers checks that the address grammar changed
// nothing for a reference without '@': every tier that answered before still
// answers, and a legacy alias containing '@' is reached by its exact name.
func TestAddressWithoutAtKeepsTiers(t *testing.T) {
	st := newStore(t)
	named(t, st, "s-kipa", "kiparra", "CORE/relay-work")
	if _, err := st.DB().ExecContext(t.Context(),
		"INSERT INTO aliases(alias, session_id, root, tool, updated_at) VALUES ('old@box', 's-kipa', ?, 'claude', ?)",
		workspace, st.Now()-1); err != nil {
		t.Fatalf("seed legacy alias: %v", err)
	}

	for _, tc := range []struct{ target, want string }{
		{"kiparra", "kiparra"},    // exact alias
		{"s-kipa", "kiparra"},     // session id
		{"relay-work", "kiparra"}, // pm ref suffix
		{"ipar", "kiparra"},       // live substring
		{"old@box", "old@box"},    // legacy alias with '@', exact only
	} {
		res, err := Send(t.Context(), st, Request{Target: tc.target, Body: "x", Human: true, Probe: alive})
		if err != nil {
			t.Errorf("Send(%q): %v", tc.target, err)
			continue
		}
		if res.Address != tc.want {
			t.Errorf("Send(%q) went to %q, want %q", tc.target, res.Address, tc.want)
		}
	}

	// A fragment of the legacy name does not reach it through the wider tiers,
	// and is not mistaken for a peer either.
	if _, err := Send(t.Context(), st, Request{Target: "d@box", Body: "x", Human: true, Probe: alive}); !errors.Is(err, deliver.ErrUnknownPeer) {
		t.Errorf("Send(d@box): err = %v, want ErrUnknownPeer", err)
	}
}

// TestRemoteSendNeverPokes runs with wake genuinely enabled, against a HOME
// with no host surfaces in it, so a poke attempt is observable: wake reaches
// nothing and says so. The local control proves that line appears when wake
// runs; the remote send must not produce it.
func TestRemoteSendNeverPokes(t *testing.T) {
	t.Setenv("PAGER_WAKE", "on")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	st := newStore(t)
	named(t, st, "s-kipa", "kipa", "")
	if _, err := deliver.AddPeer(t.Context(), st, "vm", otherStore); err != nil {
		t.Fatalf("add peer: %v", err)
	}

	local, err := Send(t.Context(), st, Request{Target: "kipa", Body: "x", Human: true, Probe: alive})
	if err != nil {
		t.Fatalf("local send: %v", err)
	}
	if !slices.ContainsFunc(local.Lines, func(l string) bool { return strings.Contains(l, "not poked") }) {
		t.Fatalf("control: a local send with wake on said %q — wake did not run, so this test proves nothing", local.Lines)
	}

	remote, err := Send(t.Context(), st, Request{Target: "kipa@vm", Body: "x", Human: true, Probe: alive})
	if err != nil {
		t.Fatalf("remote send: %v", err)
	}
	want := []string{"queued for peer vm; it leaves when the relay next runs"}
	if !slices.Equal(remote.Lines, want) {
		t.Errorf("remote send lines = %q, want %q", remote.Lines, want)
	}
	if got := remote.Head(); got != "sent #2 to kipa@vm (hop 0, human)" {
		t.Errorf("remote head = %q", got)
	}
}

// TestSendReportsTheMessageWhenLaterLookupsFail: once a message is stored, a
// failure in the steps that only report on it still hands back what was sent,
// so neither front end leaves the sender thinking nothing happened — which is
// what invites a retry that sends it twice. The sessions table is taken away
// after setup so the presence lookup, which runs after the write, fails.
func TestSendReportsTheMessageWhenLaterLookupsFail(t *testing.T) {
	st := newStore(t)
	named(t, st, "s-kipa", "kipa", "")
	if _, err := st.DB().ExecContext(t.Context(), "ALTER TABLE sessions RENAME TO sessions_gone"); err != nil {
		t.Fatalf("break the presence lookup: %v", err)
	}

	res, err := Send(t.Context(), st, Request{Target: "kipa", Body: "x", Human: true, Probe: alive})
	if err == nil {
		t.Fatal("Send succeeded although the presence lookup could not run")
	}
	if res.Sent.ID == 0 || res.Head() != "sent #1 to kipa (hop 0, human)" {
		t.Errorf("result after a late failure = %+v (head %q); want the stored message", res, res.Head())
	}
}
