package wake

import (
	"context"
	"strings"
	"syscall"
	"testing"

	"github.com/unghee/pager/internal/deliver"
)

// stub is a surface that answers however a test needs and counts what it was
// asked to do. Nothing here touches the host: these tests must behave the same
// on a machine with live sessions and one with none.
type stub struct {
	label     string
	reachable bool
	errs      []error // returned in order, one per poke; nil means success
	reaches   int
	pokes     int
	gotBody   string
}

func (s *stub) name() string { return s.label }

func (s *stub) reach(string) bool {
	s.reaches++
	return s.reachable
}

func (s *stub) poke(_ context.Context, _, body string) error {
	s.pokes++
	s.gotBody = body
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

// useAdapters swaps the surface list for one test and puts it back after.
func useAdapters(t *testing.T, list ...waker) {
	t.Helper()
	prev := adapters
	adapters = func() []waker { return list }
	t.Cleanup(func() { adapters = prev })
}

func TestNoSurfaceIsQueued(t *testing.T) {
	s := &stub{label: "stub", reachable: false}
	useAdapters(t, s)

	got := Wake(context.Background(), "s1", "body")

	if got.Outcome != Queued {
		t.Errorf("Outcome = %q, want %q", got.Outcome, Queued)
	}
	if s.pokes != 0 {
		t.Errorf("poked %d times, want 0 — an unreachable surface must not be written to", s.pokes)
	}
	if got.Via != "" {
		t.Errorf("Via = %q, want empty when nothing was tried", got.Via)
	}
}

// TestDisabledNeverPokes covers the switch the test pins depend on. If off ever
// stopped meaning off, every package that runs a send would start reaching for
// the surrounding host mid-test.
func TestDisabledNeverPokes(t *testing.T) {
	t.Setenv("PAGER_WAKE", "off")
	s := &stub{label: "stub", reachable: true}
	useAdapters(t, s)

	got := Wake(context.Background(), "s1", "body")

	if got.Outcome != Disabled {
		t.Errorf("Outcome = %q, want %q", got.Outcome, Disabled)
	}
	if s.reaches != 0 || s.pokes != 0 {
		t.Errorf("reached %d, poked %d — want 0 and 0; off must not observe the host either",
			s.reaches, s.pokes)
	}
}

func TestDisabledIsSilent(t *testing.T) {
	if line := Describe(Result{Outcome: Disabled}, "niso"); line != "" {
		t.Errorf("Describe(Disabled) = %q, want empty — the caller prints nothing", line)
	}
}

// TestFirstReachableWins fixes the selection rule: ask each surface whether it
// can carry this poke, take the first that says yes. Nothing consults the tool
// recorded for the session.
func TestFirstReachableWins(t *testing.T) {
	first := &stub{label: "first", reachable: false}
	second := &stub{label: "second", reachable: true}
	useAdapters(t, first, second)

	got := Wake(context.Background(), "s1", "body")

	if got.Outcome != Woken || got.Via != "second" {
		t.Errorf("got %q via %q, want %q via %q", got.Outcome, got.Via, Woken, "second")
	}
	if first.pokes != 0 {
		t.Errorf("unreachable surface was poked %d times", first.pokes)
	}
	if second.pokes != 1 {
		t.Errorf("reachable surface poked %d times, want 1", second.pokes)
	}
}

// TestBusyIsRetriedOnce covers the one failure worth retrying: a pipe that is
// momentarily busy is still a live peer.
func TestBusyIsRetriedOnce(t *testing.T) {
	s := &stub{label: "stub", reachable: true, errs: []error{syscall.EAGAIN}}
	useAdapters(t, s)

	got := Wake(context.Background(), "s1", "body")

	if got.Outcome != Woken {
		t.Errorf("Outcome = %q, want %q after a retryable failure", got.Outcome, Woken)
	}
	if s.pokes != 2 {
		t.Errorf("poked %d times, want 2 (first busy, then retried)", s.pokes)
	}
}

// TestGoneIsNotRetried is the other half: a peer that vanished will still be
// gone, so retrying only spends the caller's deadline.
func TestGoneIsNotRetried(t *testing.T) {
	s := &stub{label: "stub", reachable: true, errs: []error{syscall.ECONNREFUSED}}
	useAdapters(t, s)

	got := Wake(context.Background(), "s1", "body")

	if got.Outcome != Queued {
		t.Errorf("Outcome = %q, want %q", got.Outcome, Queued)
	}
	if s.pokes != 1 {
		t.Errorf("poked %d times, want 1 — a gone peer must not be retried", s.pokes)
	}
}

// TestRefusalIsReportedSeparately keeps the one signal that means the host's
// contract moved from being folded into the ordinary Queued case.
func TestRefusalIsReportedSeparately(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"explicit", ErrRefused},
		{"permission", syscall.EACCES},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &stub{label: "uds", reachable: true, errs: []error{tc.err}}
			useAdapters(t, s)

			got := Wake(context.Background(), "s1", "body")

			if got.Outcome != Refused {
				t.Errorf("Outcome = %q, want %q", got.Outcome, Refused)
			}
			if got.Err == nil {
				t.Error("Err = nil, want the underlying failure for diagnosis")
			}
		})
	}
}

func TestEmptySessionIsQueued(t *testing.T) {
	s := &stub{label: "stub", reachable: true}
	useAdapters(t, s)

	if got := Wake(context.Background(), "", "body"); got.Outcome != Queued {
		t.Errorf("Outcome = %q, want %q for an unresolvable target", got.Outcome, Queued)
	}
	if s.reaches != 0 {
		t.Errorf("reached %d times for an empty session id, want 0", s.reaches)
	}
}

// TestPokeBodyCarriesSentinel is what keeps a poke from resetting the
// recipient's causal chain. hookio looks for exactly this marker.
func TestPokeBodyCarriesSentinel(t *testing.T) {
	body := PokeBody("niso", "zuka")

	if !strings.Contains(body, deliver.PokeSentinel) {
		t.Errorf("PokeBody() = %q, want it to contain %q", body, deliver.PokeSentinel)
	}
	if !strings.Contains(body, "niso") || !strings.Contains(body, "zuka") {
		t.Errorf("PokeBody() = %q, want the alias and sender named", body)
	}
}

// TestPokeBodyNamesRecordingReader guards the hint. `pager ls` does not record
// a read, so a hint pointing only there leaves the mail pending and the next
// hook injects it again.
func TestPokeBodyNamesRecordingReader(t *testing.T) {
	if body := PokeBody("niso", "zuka"); !strings.Contains(body, "msg_list") {
		t.Errorf("PokeBody() = %q, want it to name msg_list", body)
	}
}

// TestDescribeDoesNotClaimThePeerActed holds the line D2 draws. Writing the
// frame is all this process observes; the recipient's hook is what proves a
// delivery.
func TestDescribeDoesNotClaimThePeerActed(t *testing.T) {
	line := Describe(Result{Outcome: Woken, Via: "uds"}, "niso")

	if line == "" {
		t.Fatal("Describe(Woken) = empty, want a line naming the target")
	}
	for _, claim := range []string{"delivered", "read", "received"} {
		if strings.Contains(strings.ToLower(line), claim) {
			t.Errorf("Describe(Woken) = %q, must not claim %q — there is no read step", line, claim)
		}
	}
}
