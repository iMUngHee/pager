package hookio

import (
	"os"
	"testing"

	"github.com/unghee/pager/internal/sessionref"
)

// TestMain pins host detection off for every test in this package.
//
// Run reads the host from sessionref.Detect, which walks the real process
// ancestry, so without this a test's outcome depends on what launched
// `go test`. That is how the orphan-hint tests came to pass only below a claude
// process: the fixtures seed a workspace tool, and any ancestry that resolved a
// different one overwrote the seed. Pinning makes the stored workspace — the
// thing eligibility is actually judged on — identical under either host and
// under none.
//
// Only pinned when unset, matching sessionref's own TestMain: a harness that
// re-executes a test binary has to stay in charge of its children, and a
// developer who exports PAGER_CLIENT gets the value they asked for. See
// internal/sessionref/hostpin_test.go for the measurement behind that.
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
