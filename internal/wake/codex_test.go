package wake

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rollouts builds a synthetic copy of the codex session tree. As with the
// Claude registry, no test may read the real one.
func rollouts(t *testing.T, threadIDs ...string) *codexWaker {
	t.Helper()
	root := t.TempDir()
	day := filepath.Join(root, "2026", "08", "21")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, id := range threadIDs {
		name := codexRolloutPrefix + "2026-08-21T14-29-03-" + id + codexRolloutSuffix
		if err := os.WriteFile(filepath.Join(day, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write rollout: %v", err)
		}
	}
	return &codexWaker{
		sessionsDir: root,
		queue: func(context.Context, string, string) error {
			t.Error("queue ran without being asked to")
			return nil
		},
	}
}

// TestCodexReachNeedsRollout is what keeps a socketless Claude target from
// spawning a codex subprocess on every send. Without the check reach would
// always say yes and every such target would pay for a process that then fails.
func TestCodexReachNeedsRollout(t *testing.T) {
	// Shaped like a real thread id, but not one — these fixtures must not
	// carry identifiers from whatever machine the tests were written on.
	const (
		withRollout    = "01a00000-0000-7000-8000-00000000beef"
		withoutRollout = "01a00000-0000-7000-8000-0000000000ff"
	)
	w := rollouts(t, withRollout)

	if !w.reach(withRollout) {
		t.Error("reach() = false for a thread with a rollout")
	}
	if w.reach(withoutRollout) {
		t.Error("reach() = true for a thread with no rollout — a freshly opened session has none")
	}
}

// TestCodexReachMatchesWholeID guards against a suffix match that would accept
// a different thread whose id happens to end the same way.
func TestCodexReachMatchesWholeID(t *testing.T) {
	w := rollouts(t, "aaaa-full-thread-id")

	if w.reach("full-thread-id") {
		t.Error("reach() = true for a partial id — the separator before the id must be part of the match")
	}
}

func TestCodexReachHandlesMissingTree(t *testing.T) {
	w := &codexWaker{sessionsDir: filepath.Join(t.TempDir(), "absent")}

	if w.reach("anything") {
		t.Error("reach() = true with no session tree present")
	}
}

func TestCodexPokeCarriesThreadAndBody(t *testing.T) {
	var gotThread, gotBody string
	w := rollouts(t, "t1")
	w.queue = func(_ context.Context, thread, body string) error {
		gotThread, gotBody = thread, body
		return nil
	}

	if err := w.poke(context.Background(), "t1", "poke body"); err != nil {
		t.Fatalf("poke: %v", err)
	}
	if gotThread != "t1" {
		t.Errorf("thread = %q, want %q", gotThread, "t1")
	}
	if gotBody != "poke body" {
		t.Errorf("body = %q, want %q", gotBody, "poke body")
	}
}

// TestCodexPokeFailureIsQueued keeps a failed queue from being reported as
// anything louder than it is. The mail is already stored; a codex that would
// not take the poke costs latency, not delivery.
func TestCodexPokeFailureIsQueued(t *testing.T) {
	w := rollouts(t, "t1")
	w.queue = func(context.Context, string, string) error {
		return errors.New("no rollout found for thread id t1")
	}

	err := w.poke(context.Background(), "t1", "body")
	if err == nil {
		t.Fatal("poke() = nil, want the queue failure surfaced")
	}
	if got := classify(err); got != Queued {
		t.Errorf("classify(%v) = %q, want %q", err, got, Queued)
	}
}

// TestCodexPokeIsBounded fixes the subprocess timeout. A wedged codex must not
// spend the caller's whole wake budget.
func TestCodexPokeIsBounded(t *testing.T) {
	w := rollouts(t, "t1")
	w.queue = func(ctx context.Context, _, _ string) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("queue ran without a deadline")
			return nil
		}
		if remaining := time.Until(deadline); remaining > codexQueueTimeout {
			t.Errorf("deadline is %v away, want at most %v", remaining, codexQueueTimeout)
		}
		return nil
	}

	if err := w.poke(context.Background(), "t1", "body"); err != nil {
		t.Fatalf("poke: %v", err)
	}
}
