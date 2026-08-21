package sessionref

import (
	"os"
	"testing"
)

// TestMain pins host detection off for every test in this package.
//
// Detect walks the real process ancestry, so a test that reaches it inherits
// whatever launched `go test` — green below a claude process, red in a plain
// terminal, and green for the wrong reason wherever nothing is found. That is
// not a hazard worth detecting on a second run; it is one worth making
// impossible, so the default here is no host and a test that wants one builds
// its own ancestry (TestDetectHostThroughShell).
//
// The condition is load-bearing rather than defensive. That harness re-executes
// this binary three levels deep, so TestMain runs inside the probe too, and an
// unconditional Setenv would throw away the pin the harness just passed it —
// measured: the probe reports no host and the positive case fails. Honouring an
// existing value leaves the harness in charge of its own children, and leaves a
// developer who exports PAGER_CLIENT with the value they asked for.
func TestMain(m *testing.M) {
	if os.Getenv("PAGER_CLIENT") == "" {
		os.Setenv("PAGER_CLIENT", "none")
	}
	os.Exit(m.Run())
}

// TestAmbientDetectionIsPinnedOff guards the TestMain above.
//
// Without it TestMain is a guard nobody checks: delete it and every test in
// this package quietly goes back to inheriting its ancestry, which is the
// failure this file exists to prevent and which stays invisible under an agent
// shell.
//
// Both halves are needed. The Detect check is the outcome the pin is for, but
// on its own it is blind in exactly the environment that hid the original bug —
// measured: with TestMain removed it fails under a claude ancestor and passes
// hostless, so CI would certify a missing guard. The environment check fails
// everywhere, because an unset variable is the thing being caught. Testing only
// the normalisation would not do it either: Normalize("") is already Unknown,
// so absent would read as pinned.
func TestAmbientDetectionIsPinnedOff(t *testing.T) {
	pin := os.Getenv("PAGER_CLIENT")
	if pin == "" {
		t.Fatal("PAGER_CLIENT is unset, so detection falls back to the process ancestry — TestMain must pin it")
	}
	if Normalize(pin) != Unknown {
		t.Fatalf("PAGER_CLIENT=%q names a host pager knows, so these tests can detect one", pin)
	}
	if _, host, ok := Detect(); ok || host.Valid() {
		t.Fatalf("detection is live despite PAGER_CLIENT=%q: host=%+v ok=%v", pin, host, ok)
	}
}
