package wake

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/iMUngHee/pager/internal/deliver"
)

// Sockets live under a short directory on purpose. A unix socket path is capped
// near 104 bytes and t.TempDir() on macOS is nowhere near short enough — a test
// that ignored this would fail with a bind error that looks like anything but a
// path length problem.
//
// /tmp is the short one where it exists. Windows has no /tmp at all, so there
// the base is left to MkdirTemp, which uses the platform's own temp directory:
// still far shorter than t.TempDir(), whose length comes from appending the
// test's name.
func shortDir(t *testing.T) string {
	t.Helper()
	base := "/tmp"
	if runtime.GOOS == "windows" {
		base = ""
	}
	dir, err := os.MkdirTemp(base, "pw")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// registry builds a synthetic copy of the host's session directory. Tests must
// never read the real one: this machine has live sessions, and a test that
// found one would pass or fail on what happens to be running.
type registry struct {
	dir string
}

func newRegistry(t *testing.T) *registry {
	t.Helper()
	return &registry{dir: shortDir(t)}
}

func (r *registry) record(t *testing.T, pid int, sessionID, sock string, proto int) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"pid":                 pid,
		"sessionId":           sessionID,
		"messagingSocketPath": sock,
		"peerProtocol":        proto,
	})
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	path := filepath.Join(r.dir, fmt.Sprintf("%d.json", pid))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
}

func (r *registry) key(t *testing.T, pid int, sock, token string) {
	t.Helper()
	abs, err := filepath.Abs(sock)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	sum := sha256.Sum256([]byte(abs))
	body, err := json.Marshal(map[string]string{"peerToken": token})
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	path := filepath.Join(r.dir, fmt.Sprintf("%d.%x.key", pid, sum))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

func (r *registry) waker() *claudeWaker { return &claudeWaker{dir: r.dir} }

// listener stands in for a session's inbox and reports what it was sent.
type listener struct {
	path  string
	lines chan string
}

func listenUnix(t *testing.T, dir string) *listener {
	t.Helper()
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	l := &listener{path: path, lines: make(chan string, 8)}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			select {
			case l.lines <- scanner.Text():
			default:
			}
		}
	}()
	return l
}

func (l *listener) next(t *testing.T) string {
	t.Helper()
	select {
	case line := <-l.lines:
		return line
	case <-time.After(2 * time.Second):
		t.Fatal("listener received nothing")
		return ""
	}
}

// --- reach ---------------------------------------------------------------

func TestReachFindsSessionByID(t *testing.T) {
	r := newRegistry(t)
	r.record(t, 4242, "s-alive", "/tmp/pw-nonexistent/s.sock", claudePeerProtocol)

	if !r.waker().reach("s-alive") {
		t.Error("reach() = false, want true for a recorded session with an inbox")
	}
	if r.waker().reach("s-other") {
		t.Error("reach() = true for a session id that is not recorded")
	}
}

// TestPidReuseDoesNotMatch is the property the design leans on instead of
// comparing process start times: lookup is by session id, so a recycled pid
// belongs to a record naming someone else and never matches.
func TestPidReuseDoesNotMatch(t *testing.T) {
	r := newRegistry(t)
	r.record(t, 4242, "s-new-owner", "/tmp/pw-nonexistent/s.sock", claudePeerProtocol)

	if r.waker().reach("s-previous-owner") {
		t.Error("reach() = true — a record whose pid was reused must not match the old session")
	}
}

func TestUnknownPeerProtocolIsUnreachable(t *testing.T) {
	r := newRegistry(t)
	r.record(t, 4242, "s1", "/tmp/pw-nonexistent/s.sock", claudePeerProtocol+1)

	if r.waker().reach("s1") {
		t.Error("reach() = true for an unfamiliar peerProtocol — the only version signal must gate this")
	}
}

func TestSessionWithoutInboxIsUnreachable(t *testing.T) {
	r := newRegistry(t)
	r.record(t, 4242, "s1", "", claudePeerProtocol)

	if r.waker().reach("s1") {
		t.Error("reach() = true for a session with no messaging socket recorded")
	}
}

// TestStaleSocketIsUnreachable reproduces a shape this machine actually
// produced: a socket file left behind with no record beside it. The file
// existing is not evidence its owner is alive.
func TestStaleSocketIsUnreachable(t *testing.T) {
	r := newRegistry(t)
	sockDir := shortDir(t)
	listenUnix(t, sockDir) // a real socket file, but nothing in the registry

	if r.waker().reach("s-ghost") {
		t.Error("reach() = true with a socket present but no session record")
	}
}

// --- poke ----------------------------------------------------------------

func TestPokeAuthenticatesThenSends(t *testing.T) {
	r := newRegistry(t)
	dir := shortDir(t)
	l := listenUnix(t, dir)
	r.record(t, os.Getpid(), "s1", l.path, claudePeerProtocol)
	r.key(t, os.Getpid(), l.path, "deadbeef")

	w := r.waker()
	if !w.reach("s1") {
		t.Fatal("reach() = false for a fully recorded session")
	}
	ctx, cancel := context.WithTimeout(context.Background(), Deadline)
	defer cancel()
	if err := w.poke(ctx, "s1", "body "+deliver.PokeSentinel); err != nil {
		t.Fatalf("poke: %v", err)
	}

	var auth struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(l.next(t)), &auth); err != nil {
		t.Fatalf("first line is not JSON: %v", err)
	}
	if auth.Type != "auth" {
		t.Errorf("first line type = %q, want \"auth\" — anything else on an unauthenticated connection is dropped", auth.Type)
	}
	if auth.Token != "deadbeef" {
		t.Errorf("token = %q, want the one from the key file", auth.Token)
	}

	var user struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(l.next(t)), &user); err != nil {
		t.Fatalf("second line is not JSON: %v", err)
	}
	if user.Type != "user" || user.Message.Role != "user" {
		t.Errorf("second line = %+v, want a user message", user)
	}
	if !strings.Contains(user.Message.Content, "[pager-poke]") {
		t.Errorf("content = %q, want the poke body verbatim", user.Message.Content)
	}
}

// TestClaudeFrameCarriesPriorityNow pins a field that nothing else here would
// miss. The plan wrote the frame down as measured and this field was not in
// it, so it reached the wire with no record of where it came from — see
// claudeFrame for that record. Pinning it makes dropping or renaming it a
// decision rather than a diff nobody reads.
func TestClaudeFrameCarriesPriorityNow(t *testing.T) {
	frame, err := claudeFrame("tok", "body")
	if err != nil {
		t.Fatalf("claudeFrame: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(frame), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("frame has %d lines, want 2 (auth, then the message)", len(lines))
	}

	var user struct {
		Priority string `json:"priority"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &user); err != nil {
		t.Fatalf("message line is not JSON: %v", err)
	}
	if user.Priority != "now" {
		t.Errorf("priority = %q, want \"now\" — the host declares the field over [\"now\",\"next\",\"later\"]", user.Priority)
	}
}

// TestMissingKeyFileIsRefused covers the one failure worth telling the sender
// about. It is only meaningful after a connect: the socket, the key and the
// record are written at three different moments, so a missing key on its own
// could just be a session still starting.
func TestMissingKeyFileIsRefused(t *testing.T) {
	r := newRegistry(t)
	dir := shortDir(t)
	l := listenUnix(t, dir)
	r.record(t, os.Getpid(), "s1", l.path, claudePeerProtocol)
	// no key written

	w := r.waker()
	w.reach("s1")
	err := w.poke(context.Background(), "s1", "body")
	if err == nil {
		t.Fatal("poke() = nil, want a refusal when the key is absent")
	}
	if got := classify(err); got != Refused {
		t.Errorf("classify(%v) = %q, want %q", err, got, Refused)
	}
}

// TestPeerPidMismatchIsQueued covers what the endpoint check actually buys:
// not protection from pid reuse — lookup by session id already gives that —
// but from a third party that created the socket path first.
func TestPeerPidMismatchIsQueued(t *testing.T) {
	r := newRegistry(t)
	dir := shortDir(t)
	l := listenUnix(t, dir)

	// The listener is this test process; the record claims someone else owns
	// it, which is what a squatted path looks like from here.
	r.record(t, os.Getpid()+1, "s1", l.path, claudePeerProtocol)
	r.key(t, os.Getpid()+1, l.path, "deadbeef")

	w := r.waker()
	if !w.reach("s1") {
		t.Fatal("reach() = false")
	}
	err := w.poke(context.Background(), "s1", "body")

	if err == nil {
		// peercred_other: the kernel lookup is unavailable, so the check
		// cannot run and the token stays the only lock. Nothing to assert.
		t.Skip("peer credentials unavailable on this platform — endpoint check does not run")
	}
	if got := classify(err); got != Queued {
		t.Errorf("classify(%v) = %q, want %q — a wrong endpoint is not a contract change", err, got, Queued)
	}
	select {
	case line := <-l.lines:
		t.Errorf("wrote %q to a mismatched endpoint; nothing may be sent", line)
	default:
	}
}

// TestBrokenPipeIsQueued fixes the classification D2 settled on. A rejected
// authentication and a peer that just exited both arrive as the server closing
// the connection, so this must not pretend to tell them apart.
func TestBrokenPipeIsQueued(t *testing.T) {
	for _, err := range []error{syscall.EPIPE, syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ENOENT} {
		if got := classify(err); got != Queued {
			t.Errorf("classify(%v) = %q, want %q", err, got, Queued)
		}
	}
}

func TestClaudePathHasDeadline(t *testing.T) {
	r := newRegistry(t)
	dir := shortDir(t)
	l := listenUnix(t, dir)
	r.record(t, os.Getpid(), "s1", l.path, claudePeerProtocol)
	r.key(t, os.Getpid(), l.path, "deadbeef")

	useAdapters(t, r.waker())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already past its deadline before the dial starts

	start := time.Now()
	got := Wake(ctx, "s1", "body")
	elapsed := time.Since(start)

	if got.Outcome != Queued {
		t.Errorf("Outcome = %q, want %q when the caller's context is done", got.Outcome, Queued)
	}
	if elapsed >= Deadline {
		t.Errorf("took %v, want well under the %v deadline — the context must reach the dial", elapsed, Deadline)
	}
}

// TestKeyPathIsDerivedFromTheSocketPath pins the derivation. Getting it wrong
// reads someone else's key, or none, and the failure would look like a refusal
// rather than a bug here.
func TestKeyPathIsDerivedFromTheSocketPath(t *testing.T) {
	r := newRegistry(t)
	dir := shortDir(t)
	l := listenUnix(t, dir)
	r.record(t, os.Getpid(), "s1", l.path, claudePeerProtocol)
	r.key(t, os.Getpid(), l.path, "the-right-token")
	// A key for a different socket path must not be picked up.
	r.key(t, os.Getpid(), filepath.Join(dir, "other.sock"), "the-wrong-token")

	w := r.waker()
	w.reach("s1")
	token, err := w.peerToken(w.found)
	if err != nil {
		t.Fatalf("peerToken: %v", err)
	}
	if token != "the-right-token" {
		t.Errorf("token = %q, want the key named for this socket", token)
	}
}
