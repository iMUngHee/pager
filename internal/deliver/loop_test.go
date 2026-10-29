package deliver

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iMUngHee/pager/internal/clock"
	"github.com/iMUngHee/pager/internal/store"
)

// JSON keeps these regressions runnable against the old request shape: old
// binaries ignore the new optional fields and expose their actual behavior.
func loopSend(t *testing.T, st *store.Store, sender, alias, body string, meta map[string]string) Sent {
	t.Helper()
	v := map[string]string{"Sender": sender, "Alias": alias, "Body": body}
	for k, value := range meta {
		v[k] = value
	}
	b, _ := json.Marshal(v)
	var req SendRequest
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	sent, err := Send(t.Context(), st, req)
	if err != nil {
		t.Fatalf("send %s -> %s: %v", sender, alias, err)
	}
	return sent
}

func loopPause(sent Sent) string {
	b, _ := json.Marshal(sent)
	var receipt struct{ PauseReason string }
	_ = json.Unmarshal(b, &receipt)
	return receipt.PauseReason
}

func TestLoopNormalWorkExceedsFormerHopAndHourlyCaps(t *testing.T) {
	st, _ := newStore(t)
	twoSessions(t, st)
	for i := 0; i < 45; i++ {
		sender, target, receiver := "A", "inboxB", "B"
		if i%2 == 1 {
			sender, target, receiver = "B", "inboxA", "A"
		}
		s := loopSend(t, st, sender, target, fmt.Sprintf("step %d: changed result", i), map[string]string{"activity": "working"})
		if loopPause(s) != "" {
			t.Fatalf("real work paused: %+v", s)
		}
		deliverAndConfirm(t, st, receiver, fmt.Sprintf("normal-%d", i), s.ID)
	}
}

func TestLoopUnknownSignalsAndUncausedSendsRemainAllowed(t *testing.T) {
	st, _ := newStore(t)
	twoSessions(t, st)
	for i := 0; i < 45; i++ {
		s := loopSend(t, st, "A", "inboxB", "same wrapper message", nil)
		if loopPause(s) != "" {
			t.Fatal("missing signals were treated as no work")
		}
	}
}

type pausedLoop struct {
	st            *store.Store
	fake          *clock.Fake
	sent          Sent
	sender, alias string
}

func makePausedLoop(t *testing.T) pausedLoop {
	t.Helper()
	t.Setenv("PAGER_LOOP_IDLE", "2s")
	st, fake := newStore(t)
	twoSessions(t, st)
	for i := 0; i < 16; i++ {
		sender, alias, receiver, body := "A", "inboxB", "B", "please continue"
		if i%2 == 1 {
			sender, alias, receiver, body = "B", "inboxA", "A", "acknowledged"
		}
		s := loopSend(t, st, sender, alias, body, map[string]string{"activity": "idle", "task": "job"})
		if loopPause(s) != "" {
			waiting, err := List(t.Context(), st, receiver, FilterWaiting, DefaultLimits())
			if err != nil || len(waiting) == 0 {
				t.Fatalf("paused mail lost: waiting=%v, error=%v", waiting, err)
			}
			return pausedLoop{st, fake, s, sender, alias}
		}
		deliverAndConfirm(t, st, receiver, fmt.Sprintf("idle-%d", i), s.ID)
		fake.Advance(500 * time.Millisecond)
	}
	t.Fatal("confirmed idle repeated exchange never paused")
	return pausedLoop{}
}

func TestLoopIdlePingPongPausesWakeButRetainsMail(t *testing.T) { _ = makePausedLoop(t) }

func TestLoopCriticalEventsBypassPauseAndAreIdempotent(t *testing.T) {
	for _, kind := range []string{"result", "failure", "release"} {
		t.Run(kind, func(t *testing.T) {
			p := makePausedLoop(t)
			meta := map[string]string{"activity": "idle", "task": "job", "event_kind": kind, "event_id": "gpu-event"}
			s := loopSend(t, p.st, p.sender, p.alias, p.sentBody(t), meta)
			if loopPause(s) != "" {
				t.Fatalf("important %s paused", kind)
			}
			retry := loopSend(t, p.st, p.sender, p.alias, p.sentBody(t), meta)
			if retry.ID != s.ID {
				t.Fatalf("event retry wrote another message: %d != %d", retry.ID, s.ID)
			}
		})
	}
}

func TestLoopImportantMailPrecedesPausedBacklog(t *testing.T) {
	p := makePausedLoop(t)
	for i := 0; i < 10; i++ {
		loopSend(t, p.st, p.sender, p.alias, p.sentBody(t), map[string]string{"activity": "idle", "task": "job"})
	}
	critical := loopSend(t, p.st, p.sender, p.alias, "GPU released", map[string]string{"event_kind": "release", "task": "job"})
	receiver := "A"
	if p.sender == "A" {
		receiver = "B"
	}
	lim := DefaultLimits()
	lim.MaxBatch = 1
	b := collect(t, p.st, receiver, lim)
	if len(b.Messages) != 1 || b.Messages[0].ID != critical.ID {
		t.Fatalf("important event %d stuck behind paused updates: %+v", critical.ID, b.Messages)
	}
}

func TestLoopPauseReasonReachesHookAndArchive(t *testing.T) {
	p := makePausedLoop(t)
	receiver := "A"
	if p.sender == "A" {
		receiver = "B"
	}
	if got := Render(collect(t, p.st, receiver, DefaultLimits())); !strings.Contains(got, "automatic wake paused") {
		t.Fatalf("recipient cannot see pause reason: %s", got)
	}
	var out bytes.Buffer
	if _, err := p.st.ExportAll(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"wake_note"`) || !strings.Contains(out.String(), `"activity":"idle"`) {
		t.Fatalf("archive lost loop metadata: %s", out.String())
	}
}

func TestLoopEventRetriesAreAtomicAndConflictsAreExplicit(t *testing.T) {
	st, _ := newStore(t)
	twoSessions(t, st)
	var wg sync.WaitGroup
	ids := make(chan int64, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := loopSend(t, st, "A", "inboxB", "GPU free", map[string]string{"event_kind": "release", "event_id": "lease-1"})
			ids <- s.ID
		}()
	}
	wg.Wait()
	close(ids)
	var first int64
	for id := range ids {
		if first == 0 {
			first = id
		}
		if id != first {
			t.Fatalf("concurrent retry wrote extra event %d != %d", id, first)
		}
	}
	_, err := Send(t.Context(), st, SendRequest{Sender: "A", Alias: "inboxB", Body: "different transition", Metadata: Metadata{Kind: "release", EventID: "lease-1"}})
	if err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("conflicting event ID error=%v", err)
	}
}

func TestLoopGuardFailurePreservesMailAndFailsOpen(t *testing.T) {
	p := makePausedLoop(t)
	if _, err := p.st.DB().Exec("DROP TABLE loop_guard"); err != nil {
		t.Fatal(err)
	}
	s := loopSend(t, p.st, p.sender, p.alias, p.sentBody(t), map[string]string{"activity": "idle", "task": "job"})
	if loopPause(s) != "" || s.ID == 0 || s.Warning == "" {
		t.Fatalf("guard error lost mail or disabled waking: %+v", s)
	}
}

func TestLoopInvalidDurationDoesNotBecomeAnImmediatePause(t *testing.T) {
	for _, value := range []string{"0", "-1s", "1ns", "bad"} {
		t.Run(value, func(t *testing.T) {
			p := makePausedLoop(t)
			t.Setenv("PAGER_LOOP_IDLE", value)
			s := loopSend(t, p.st, p.sender, p.alias, p.sentBody(t), map[string]string{"activity": "idle", "task": "job"})
			if s.PauseReason != "" || s.Warning == "" {
				t.Fatalf("invalid duration %q changed delivery: %+v", value, s)
			}
		})
	}
}

func TestLoopSameBodyWithChangingArtifactCheckpointDoesNotPause(t *testing.T) {
	t.Setenv("PAGER_LOOP_IDLE", "2s")
	st, fake := newStore(t)
	twoSessions(t, st)
	path := filepath.Join(t.TempDir(), "actual-plan.txt")
	for i := 0; i < 20; i++ {
		if err := os.WriteFile(path, []byte(fmt.Sprintf("revision %d", i)), 0600); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sender, alias, receiver := "A", "inboxB", "B"
		if i%2 == 1 {
			sender, alias, receiver = "B", "inboxA", "A"
		}
		s := loopSend(t, st, sender, alias, "review the fixed path", map[string]string{"activity": "idle", "task": "review", "progress": fmt.Sprintf("artifact:%x", sha256.Sum256(content))})
		if loopPause(s) != "" {
			t.Fatal("changing real artifact was ignored")
		}
		deliverAndConfirm(t, st, receiver, fmt.Sprintf("artifact-%d", i), s.ID)
		fake.Advance(time.Second)
	}
}

func TestLoopScopeUnknownStaleAndNewEpochRecover(t *testing.T) {
	for _, change := range []string{"task", "unknown", "stale", "epoch", "progress", "working"} {
		t.Run(change, func(t *testing.T) {
			p := makePausedLoop(t)
			meta := map[string]string{"activity": "idle", "task": "job"}
			switch change {
			case "task":
				meta["task"] = "another-job"
			case "unknown":
				delete(meta, "activity")
			case "stale":
				p.fake.Advance(5 * time.Minute)
			case "epoch":
				if err := ResetCausal(t.Context(), p.st, p.sender); err != nil {
					t.Fatal(err)
				}
			case "progress":
				meta["progress"] = "test now passes"
			case "working":
				meta["activity"] = "working"
			}
			s := loopSend(t, p.st, p.sender, p.alias, p.sentBody(t), meta)
			if loopPause(s) != "" {
				t.Fatalf("%s did not recover", change)
			}
		})
	}
}

func (p pausedLoop) sentBody(t *testing.T) string {
	t.Helper()
	var body string
	if err := p.st.DB().QueryRow("SELECT body FROM messages WHERE id=?", p.sent.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	return body
}
