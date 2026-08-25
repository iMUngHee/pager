package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unghee/pager/internal/wake"
)

// TestWakePinnedOff guards the wake half of TestMain; removing that pin must
// break something here.
//
// The environment check catches a missing pin directly, and the Enabled check
// is the outcome the pin exists for — a send in this package reaching a live
// session on the machine running the tests.
func TestWakePinnedOff(t *testing.T) {
	if got := os.Getenv("PAGER_WAKE"); got != "off" {
		t.Fatalf("PAGER_WAKE = %q, want \"off\" — TestMain must pin it before any send runs", got)
	}
	if wake.Enabled() {
		t.Fatal("wake is live despite the pin; these tests would poke real sessions")
	}
}

// TestSendSurvivesWakeFailure is the property the whole design rests on: the
// database write is the delivery, and a poke only moves when the recipient
// reads it. A send whose wake finds nothing must still succeed, still store the
// message, and say plainly that the recipient was not woken.
//
// Wake runs for real here rather than pinned off — HOME points at an empty
// directory, so both adapters look for their surfaces and find none. That is
// the failure this exercises, and it exercises it without reading the host.
func TestSendSurvivesWakeFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "msg.db")
	t.Setenv("PAGER_DB", dbPath)
	t.Setenv("PAGER_WAKE", "on")
	t.Setenv("HOME", t.TempDir())

	mustRun(t, "attach", "--session", "a", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "a", "a-box")
	mustRun(t, "attach", "--session", "b", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "b", "b-box")

	out, err := capture(t, "", "send", "--session", "a", "b-box", "still delivered")
	if err != nil {
		t.Fatalf("send failed because wake could not reach the target: %v\n%s", err, out)
	}
	if !strings.Contains(out, "sent #") {
		t.Errorf("send did not report success:\n%s", out)
	}
	if !strings.Contains(out, "not poked") {
		t.Errorf("send did not say the recipient was left for its next activity:\n%s", out)
	}

	st := openDB(t, dbPath)
	var body string
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT body FROM messages ORDER BY id DESC LIMIT 1").Scan(&body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if body != "still delivered" {
		t.Errorf("body = %q, want the message stored regardless of the poke", body)
	}
}
