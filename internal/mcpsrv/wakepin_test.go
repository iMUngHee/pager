package mcpsrv

import (
	"os"
	"strings"
	"testing"

	"github.com/iMUngHee/pager/internal/wake"
)

// TestWakePinnedOff guards the wake half of TestMain; removing that pin must
// break something here. See cmd/pager/wakepin_test.go for the reasoning — the
// MCP server runs the same send path and needs the same guarantee.
func TestWakePinnedOff(t *testing.T) {
	if got := os.Getenv("PAGER_WAKE"); got != "off" {
		t.Fatalf("PAGER_WAKE = %q, want \"off\" — TestMain must pin it before any send runs", got)
	}
	if wake.Enabled() {
		t.Fatal("wake is live despite the pin; these tests would poke real sessions")
	}
}

// TestMsgSendSurvivesWakeFailure is the CLI property restated for the surface
// an agent actually calls: a tool call whose poke finds nothing must still
// report the send as done and still store the message.
//
// Wake runs for real, with HOME pointed at an empty directory so both adapters
// look for their surfaces and find none — the failure without the host reads.
func TestMsgSendSurvivesWakeFailure(t *testing.T) {
	st := newStore(t)
	seed(t, st, "sender-session", "sender-box")
	seed(t, st, "recipient-session", "recipient-box")
	t.Setenv("PAGER_SESSION", "sender-session")
	t.Setenv("PAGER_WAKE", "on")
	t.Setenv("HOME", t.TempDir())

	s := start(t, st)
	resp := s.call(2, "tools/call", map[string]any{
		"name":      "msg_send",
		"arguments": map[string]any{"target": "recipient-box", "body": "still delivered"},
	})
	if isError(t, resp) {
		t.Fatalf("msg_send failed because wake could not reach the target: %s", text(t, resp))
	}
	got := text(t, resp)
	if !strings.Contains(got, "sent #") {
		t.Errorf("result does not report the send: %s", got)
	}
	if !strings.Contains(got, "not poked") {
		t.Errorf("result does not say the recipient was left for its next activity: %s", got)
	}

	var body string
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT body FROM messages ORDER BY id DESC LIMIT 1").Scan(&body); err != nil {
		t.Fatalf("read the stored message: %v", err)
	}
	if body != "still delivered" {
		t.Errorf("body = %q, want the message stored regardless of the poke", body)
	}
}
