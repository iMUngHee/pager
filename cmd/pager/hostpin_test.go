package main

import (
	"os"
	"testing"

	"github.com/iMUngHee/pager/internal/sessionref"
)

// TestMain pins host detection and wake off for every test in this package.
//
// The CLI resolves its session through sessionref.New, whose resolver runs the
// ancestry walk on every call, so without this a test's outcome depends on what
// launched `go test` — and these are the end-to-end tests, where a session
// resolving differently changes what a send is attributed to. Only pinned when
// unset — see internal/sessionref/hostpin_test.go for why that condition
// matters.
//
// Wake needs the same treatment for the same reason, one step further out: a
// send now reaches for the surrounding host's sessions and can connect to a
// live one. A test that woke a real session would be writing into someone's
// conversation to check its own bookkeeping.
func TestMain(m *testing.M) {
	if os.Getenv("PAGER_CLIENT") == "" {
		os.Setenv("PAGER_CLIENT", "none")
	}
	if os.Getenv("PAGER_WAKE") == "" {
		os.Setenv("PAGER_WAKE", "off")
	}
	os.Exit(m.Run())
}

// TestAmbientDetectionIsPinnedOff guards the TestMain above; deleting it must
// break something. The environment check is what catches a missing pin in CI,
// where a Detect-only assertion would pass for the wrong reason, and the Detect
// check is the outcome the pin exists for.
func TestAmbientDetectionIsPinnedOff(t *testing.T) {
	pin := os.Getenv("PAGER_CLIENT")
	if pin == "" {
		t.Fatal("PAGER_CLIENT is unset, so detection falls back to the process ancestry — TestMain must pin it")
	}
	if sessionref.Normalize(pin) != sessionref.Unknown {
		t.Fatalf("PAGER_CLIENT=%q names a host pager knows, so these tests can detect one", pin)
	}
	if _, host, ok := sessionref.Detect(); ok || host.Valid() {
		t.Fatalf("detection is live despite PAGER_CLIENT=%q: host=%+v ok=%v", pin, host, ok)
	}
}
