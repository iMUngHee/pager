package wake

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Codex takes a queued message for an existing session through a documented
// subcommand, which makes this the one surface here that is not a private
// contract:
//
//	codex queue --thread <id> --message <text>
//
// The catch is that it only accepts a thread the store already knows, and a
// thread only enters the store once it has run a turn. A session opened but
// never used has nothing to queue against, and the command fails with "no
// rollout found for thread id".
const (
	// codexQueueTimeout bounds the subprocess. It is well inside the wake
	// deadline so a wedged codex cannot eat the whole budget.
	codexQueueTimeout = 2 * time.Second

	codexRolloutPrefix = "rollout-"
	codexRolloutSuffix = ".jsonl"

	// codexRolloutStampLen is the width of the timestamp between the prefix
	// and the thread id: 2026-08-21T14-29-03.
	//
	// Reading the id by position rather than by suffix is what keeps one
	// thread from matching another whose id ends the same way. Should the
	// stamp ever change width, this stops matching and every codex target
	// falls back to Queued — the hook still delivers.
	codexRolloutStampLen = len("2026-08-21T14-29-03")
)

// codexWaker reaches a codex session through its queue.
//
// Both fields exist to be replaced in tests: one so the rollout scan reads a
// synthetic tree, the other so no test ever runs the real binary.
type codexWaker struct {
	sessionsDir string
	queue       func(ctx context.Context, thread, body string) error
}

func newCodexWaker() *codexWaker {
	w := &codexWaker{queue: runCodexQueue}
	if home, err := os.UserHomeDir(); err == nil {
		w.sessionsDir = filepath.Join(home, ".codex", "sessions")
	}
	return w
}

func (c *codexWaker) name() string { return "codex-queue" }

// reach reports whether this thread has a rollout, which is the precondition
// the queue command enforces.
//
// Observing it beforehand is what keeps a socketless Claude session from
// spawning a subprocess on every send: without this check reach would always
// say yes, and every target that the UDS adapter could not take would pay for a
// codex process that then failed.
//
// The observation is the filename. Rollouts are stored as
// sessions/YYYY/MM/DD/rollout-<timestamp>-<thread-id>.jsonl, so the id is
// readable without opening anything. The scan walks names only and stops at the
// first match; the tree on the machine this was written against held 355 files.
func (c *codexWaker) reach(sessionID string) bool {
	if c.sessionsDir == "" || sessionID == "" {
		return false
	}
	found := false
	_ = filepath.WalkDir(c.sessionsDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner is not an answer, just a gap
		}
		if d.IsDir() {
			return nil
		}
		if threadIDFromRollout(d.Name()) == sessionID {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// threadIDFromRollout pulls the thread id out of a rollout filename, or returns
// "" when the name is not one.
func threadIDFromRollout(name string) string {
	if !strings.HasPrefix(name, codexRolloutPrefix) || !strings.HasSuffix(name, codexRolloutSuffix) {
		return ""
	}
	core := name[len(codexRolloutPrefix) : len(name)-len(codexRolloutSuffix)]
	if len(core) <= codexRolloutStampLen+1 || core[codexRolloutStampLen] != '-' {
		return ""
	}
	return core[codexRolloutStampLen+1:]
}

func (c *codexWaker) poke(ctx context.Context, sessionID, body string) error {
	ctx, cancel := context.WithTimeout(ctx, codexQueueTimeout)
	defer cancel()
	return c.queue(ctx, sessionID, body)
}

// runCodexQueue shells out to the documented subcommand.
//
// The output is folded into the error because the failures worth reading are
// stated there in prose — a missing rollout, an unknown thread — and none of
// them are distinguishable from the exit status alone.
func runCodexQueue(ctx context.Context, thread, body string) error {
	cmd := exec.CommandContext(ctx, "codex", "queue", "--thread", thread, "--message", body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" {
			return fmt.Errorf("codex queue: %w", err)
		}
		return fmt.Errorf("codex queue: %w: %s", err, trimmed)
	}
	return nil
}
