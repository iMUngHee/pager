package store

import (
	"testing"
	"time"
)

const (
	testClient = "claude"
	testPid    = 4242
	testStart  = 1754280000123
)

func hostRecord(id string) SessionRecord {
	return SessionRecord{
		ID: id, Tool: testClient, Root: "/tmp/workspace",
		HostClient: testClient, HostPid: testPid, HostStart: testStart,
	}
}

func lookup(t *testing.T, s *Store, pid int, start int64) string {
	t.Helper()
	id, err := s.SessionByHost(t.Context(), testClient, pid, start, DefaultStale)
	if err != nil {
		t.Fatalf("SessionByHost: %v", err)
	}
	return id
}

func TestRecordSessionBindsHost(t *testing.T) {
	s, _ := newStore(t)
	if err := s.RecordSession(t.Context(), hostRecord("s1")); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	if got := lookup(t, s, testPid, testStart); got != "s1" {
		t.Errorf("SessionByHost = %q, want %q", got, "s1")
	}
}

// TestRecordSessionTransfersHostKey is the /clear-and-resume case: the same
// host process reports a new session id. The key must move, leaving exactly one
// holder — the unique index would reject the write outright if the old row kept
// it, and a reader must never see the superseded session.
func TestRecordSessionTransfersHostKey(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()

	if err := s.RecordSession(ctx, hostRecord("s1")); err != nil {
		t.Fatalf("record first: %v", err)
	}
	if err := s.RecordSession(ctx, hostRecord("s2")); err != nil {
		t.Fatalf("record second: %v", err)
	}

	if got := lookup(t, s, testPid, testStart); got != "s2" {
		t.Errorf("SessionByHost = %q, want the new session %q", got, "s2")
	}

	// The superseded session survives as a row — it may still own aliases and
	// undelivered mail — but no longer answers to the host key.
	var holders int
	if err := s.db.QueryRowContext(ctx,
		"SELECT count(*) FROM sessions WHERE host_pid = ?", testPid).Scan(&holders); err != nil {
		t.Fatalf("count holders: %v", err)
	}
	if holders != 1 {
		t.Errorf("%d sessions hold the host key, want 1", holders)
	}
	var exists int
	if err := s.db.QueryRowContext(ctx,
		"SELECT count(*) FROM sessions WHERE session_id = 's1'").Scan(&exists); err != nil {
		t.Fatalf("count s1: %v", err)
	}
	if exists != 1 {
		t.Error("the superseded session row was removed, want kept")
	}
}

// TestRecordSessionWithoutHostPreservesBinding covers a hook that momentarily
// cannot see its ancestry: refreshing the heartbeat must not cost the session
// the attribution it already had.
func TestRecordSessionWithoutHostPreservesBinding(t *testing.T) {
	s, fake := newStore(t)
	ctx := t.Context()

	if err := s.RecordSession(ctx, hostRecord("s1")); err != nil {
		t.Fatalf("record with host: %v", err)
	}
	fake.Advance(time.Minute)
	if err := s.RecordSession(ctx, SessionRecord{ID: "s1", Tool: testClient, Root: "/tmp/workspace"}); err != nil {
		t.Fatalf("record without host: %v", err)
	}

	if got := lookup(t, s, testPid, testStart); got != "s1" {
		t.Errorf("SessionByHost = %q, want %q — the binding was cleared", got, "s1")
	}
}

// TestSessionByHostRejectsReusedPid is why the start token exists. A later
// process inheriting the pid produces a different token, so the lookup misses
// rather than attributing a send to a dead session.
func TestSessionByHostRejectsReusedPid(t *testing.T) {
	s, _ := newStore(t)
	if err := s.RecordSession(t.Context(), hostRecord("s1")); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	if got := lookup(t, s, testPid, testStart+1); got != "" {
		t.Errorf("SessionByHost with a different start token = %q, want none", got)
	}
	if got := lookup(t, s, testPid+1, testStart); got != "" {
		t.Errorf("SessionByHost with a different pid = %q, want none", got)
	}
}

// TestSessionByHostRejectsStaleHeartbeat covers the other failure this guards:
// the host is alive but its hooks stopped running, so what is recorded can no
// longer be trusted to describe the live session.
func TestSessionByHostRejectsStaleHeartbeat(t *testing.T) {
	s, fake := newStore(t)
	if err := s.RecordSession(t.Context(), hostRecord("s1")); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}

	fake.Advance(DefaultStale - time.Minute)
	if got := lookup(t, s, testPid, testStart); got != "s1" {
		t.Errorf("just inside the window: got %q, want %q", got, "s1")
	}

	fake.Advance(2 * time.Minute) // now past DefaultStale
	if got := lookup(t, s, testPid, testStart); got != "" {
		t.Errorf("past the window: got %q, want none", got)
	}
}

func TestSessionByHostIgnoresInvalidPid(t *testing.T) {
	s, _ := newStore(t)
	for _, pid := range []int{0, 1, -1} {
		if got := lookup(t, s, pid, testStart); got != "" {
			t.Errorf("SessionByHost(pid=%d) = %q, want none", pid, got)
		}
	}
}

// TestRecordSessionRefreshesHeartbeatAndKeepsPMRef checks the two fields a
// repeat hook call must treat differently: the heartbeat always advances, while
// an omitted pm_ref leaves the stored one alone.
func TestRecordSessionRefreshesHeartbeatAndKeepsPMRef(t *testing.T) {
	s, fake := newStore(t)
	ctx := t.Context()

	rec := hostRecord("s1")
	rec.PMRef = "CORE/pager-core-delivery"
	if err := s.RecordSession(ctx, rec); err != nil {
		t.Fatalf("record: %v", err)
	}
	first := s.Now()

	fake.Advance(time.Hour)
	bare := hostRecord("s1") // no PMRef
	if err := s.RecordSession(ctx, bare); err != nil {
		t.Fatalf("re-record: %v", err)
	}

	var beat int64
	var pmRef string
	if err := s.db.QueryRowContext(ctx,
		"SELECT heartbeat_at, pm_ref FROM sessions WHERE session_id = 's1'").Scan(&beat, &pmRef); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if beat <= first {
		t.Errorf("heartbeat_at = %d, want later than %d", beat, first)
	}
	if pmRef != "CORE/pager-core-delivery" {
		t.Errorf("pm_ref = %q, want it preserved", pmRef)
	}
}

func TestRecordSessionRejectsEmptyID(t *testing.T) {
	s, _ := newStore(t)
	if err := s.RecordSession(t.Context(), SessionRecord{Tool: testClient, Root: "/tmp/w"}); err == nil {
		t.Error("recorded a session with no id, want an error")
	}
}
