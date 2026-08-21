package main

import (
	"os"
	"testing"

	"github.com/unghee/pager/internal/sessionref"
)

// TestMain pins host detection off for every test in this package.
//
// The CLI resolves its session through sessionref.New, whose resolver runs the
// ancestry walk on every call, so without this a test's outcome depends on what
// launched `go test` — and these are the end-to-end tests, where a session
// resolving differently changes what a send is attributed to. Only pinned when
// unset — see internal/sessionref/hostpin_test.go for why that condition
// matters.
func TestMain(m *testing.M) {
	if os.Getenv("PAGER_CLIENT") == "" {
		os.Setenv("PAGER_CLIENT", "none")
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
