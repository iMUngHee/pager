package relay

import (
	"os"
	"testing"

	"github.com/iMUngHee/pager/internal/sessionref"
	"github.com/iMUngHee/pager/internal/wake"
)

// TestMain pins host detection and wake off for every test in this package.
// relay in and relay ack end by poking the sessions they delivered to, and a
// test that reached a real one would be writing into someone's conversation.
// Only pinned when unset, so a test that needs wake can set PAGER_WAKE=on with
// HOME pointed at an empty directory.
func TestMain(m *testing.M) {
	if os.Getenv("PAGER_CLIENT") == "" {
		os.Setenv("PAGER_CLIENT", "none")
	}
	if os.Getenv("PAGER_WAKE") == "" {
		os.Setenv("PAGER_WAKE", "off")
	}
	os.Exit(m.Run())
}

// TestRelayPackagePinsHostAndWake guards the TestMain above; deleting it must
// break something. Each pin is checked by its variable and by its outcome.
func TestRelayPackagePinsHostAndWake(t *testing.T) {
	pin := os.Getenv("PAGER_CLIENT")
	if pin == "" || sessionref.Normalize(pin) != sessionref.Unknown {
		t.Fatalf("PAGER_CLIENT=%q — TestMain must pin it to a value naming no host", pin)
	}
	if _, host, ok := sessionref.Detect(); ok || host.Valid() {
		t.Fatalf("detection is live despite PAGER_CLIENT=%q: host=%+v ok=%v", pin, host, ok)
	}

	if got := os.Getenv("PAGER_WAKE"); got != "off" {
		t.Fatalf("PAGER_WAKE=%q — TestMain must pin it off", got)
	}
	if r := wake.Wake(t.Context(), "any-session", "body"); r.Outcome != wake.Disabled {
		t.Fatalf("wake is live despite PAGER_WAKE=off: outcome %q", r.Outcome)
	}
}
