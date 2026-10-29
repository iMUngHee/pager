package wake

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iMUngHee/pager/internal/deliver"
	"github.com/iMUngHee/pager/internal/store"
	_ "modernc.org/sqlite"
)

// Codex's queue subcommand always adds a new submission. Its native
// experimental thread/queue protocol also exposes list and delete, which
// lets pager coalesce notifications before adding another submission.
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
	if home := os.Getenv("CODEX_HOME"); home != "" {
		w.sessionsDir = filepath.Join(home, "sessions")
	} else if home, err := os.UserHomeDir(); err == nil {
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

// runCodexQueue merges notifications through the native queue protocol. A
// SQLite sidecar serializes the check/add sequence across sender processes;
// it holds no persistent pending flag, and process death releases the lock.
// The Codex queue itself is changed only through its list/delete/add API.
func runCodexQueue(ctx context.Context, thread, body string) error {
	return updateCodexQueue(ctx, thread, body, nil)
}

// ClearCodexQueue removes generated wakes after a successful read/delivery.
// Rechecking unread mail under the same lock as enqueue prevents a concurrent
// sender's new notification from being cancelled by an older read.
func ClearCodexQueue(ctx context.Context, st *store.Store, session string) error {
	if !Enabled() {
		return nil
	}
	tool, err := deliver.SenderTool(ctx, st, session)
	if err != nil || tool != "codex" {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, codexQueueTimeout)
	defer cancel()
	return updateCodexQueue(ctx, session, "", st)
}

func updateCodexQueue(ctx context.Context, thread, body string, clearing *store.Store) error {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = filepath.Join(userHome, ".codex")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	dsn := url.URL{Scheme: "file", Opaque: filepath.Join(home, "pager-wake-lock.sqlite"), RawQuery: "_pragma=busy_timeout(2000)"}
	gate, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return err
	}
	defer gate.Close()
	conn, err := gate.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("lock Codex wake queue: %w", err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	if clearing != nil {
		pending, err := deliver.List(ctx, clearing, thread, deliver.FilterWaiting, deliver.LimitsFromEnv())
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			return nil
		}
	}

	cmd := exec.CommandContext(ctx, "codex", "app-server")
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		in.Close()
		return err
	}
	defer func() { in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	enc, dec := json.NewEncoder(in), json.NewDecoder(out)
	requestID := 0
	call := func(method string, params any) (json.RawMessage, error) {
		requestID++
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": requestID, "method": method, "params": params}); err != nil {
			return nil, err
		}
		for {
			var response struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := dec.Decode(&response); err != nil {
				return nil, fmt.Errorf("Codex queue protocol: %w", err)
			}
			if response.ID == nil || *response.ID != requestID {
				continue
			}
			if response.Error != nil {
				return nil, fmt.Errorf("Codex %s: %s (%d)", method, response.Error.Message, response.Error.Code)
			}
			return response.Result, nil
		}
	}
	_, err = call("initialize", map[string]any{"clientInfo": map[string]string{"name": "pager", "version": "0.1.0"}, "capabilities": map[string]bool{"experimentalApi": true}})
	if err != nil {
		return err
	}
	if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		return err
	}
	var pending []string
	var cursor *string
	for {
		raw, err := call("thread/queue/list", map[string]any{"threadId": thread, "cursor": cursor, "limit": 100})
		if err != nil {
			return err
		}
		var page struct {
			Data *[]struct {
				ID    string `json:"id"`
				Input []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"input"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		if page.Data == nil {
			return fmt.Errorf("Codex queue/list response has no data array")
		}
		for _, item := range *page.Data {
			if len(item.Input) == 1 && item.Input[0].Type == "text" && deliver.IsPokePrompt(item.Input[0].Text) {
				if item.ID == "" {
					return fmt.Errorf("Codex queued notification has no id")
				}
				pending = append(pending, item.ID)
			}
		}
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	if len(pending) > 0 {
		keep := 1
		if clearing != nil {
			keep = 0
		}
		for _, id := range pending[keep:] {
			if _, err := call("thread/queue/delete", map[string]string{"threadId": thread, "queuedSubmissionId": id}); err != nil {
				return err
			}
		}
		if clearing == nil {
			return errAlreadyQueued
		}
	}
	if clearing != nil {
		return nil
	}
	_, err = call("thread/queue/add", map[string]any{"threadId": thread, "clientUserMessageId": "pager-" + rand.Text(), "input": []map[string]string{{"type": "text", "text": body}}})
	if err != nil {
		return err
	}
	return nil
}
