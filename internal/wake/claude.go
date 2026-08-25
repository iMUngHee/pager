package wake

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Claude Code publishes a record per live session and listens on a unix socket
// named in it. Everything this adapter needs is in that directory: the socket
// path, the pid that owns it, and the token a sender must present.
//
// None of it is a documented contract. peerProtocol is the only version signal
// the records carry, so it is the one thing checked before speaking — an
// unfamiliar value means stay quiet and let the hook deliver, which is the
// behaviour pager had all along.
const (
	claudePeerProtocol = 1

	// claudeLineCap mirrors the listener's own limit. A longer line makes it
	// drop the connection, so refusing early turns a confusing disconnect
	// into a plain error.
	claudeLineCap = 1 << 20

	// darwinLinger is how long to wait after writing before closing.
	//
	// The host's own client does the same. Closing immediately can lose the
	// frame: the listener reads line-delimited and a close racing the last
	// write costs the message with no error on this side.
	darwinLinger = 150 * time.Millisecond

	// claudeRecordCap bounds how much of a session record is read. The real
	// ones are a few hundred bytes.
	claudeRecordCap = 256 << 10
)

var claudeRecordName = regexp.MustCompile(`^\d+\.json$`)

// claudeWaker reaches a Claude Code session over its messaging socket.
//
// dir is a field rather than a lookup so tests can point it at a synthetic
// registry. Nothing else in this package may read the real one during a test.
type claudeWaker struct {
	dir string

	// found caches what reach resolved so poke does not scan twice. One
	// instance serves one Wake call.
	found *claudeSession
}

type claudeSession struct {
	pid  int
	sock string
}

// claudeRecord is the slice of a session record this adapter reads. The host
// writes many more fields; ignoring them is deliberate, since the ones read
// here are the only ones with a stated meaning.
type claudeRecord struct {
	Pid                 int    `json:"pid"`
	SessionID           string `json:"sessionId"`
	MessagingSocketPath string `json:"messagingSocketPath"`
	PeerProtocol        int    `json:"peerProtocol"`
}

func newClaudeWaker() *claudeWaker {
	home, err := os.UserHomeDir()
	if err != nil {
		return &claudeWaker{}
	}
	return &claudeWaker{dir: filepath.Join(home, ".claude", "sessions")}
}

func (c *claudeWaker) name() string { return "uds" }

// reach answers whether this session has a messaging inbox we know how to talk
// to. It reads; it never writes or connects.
//
// A session that predates the messaging feature, or one started without it,
// simply has no socket path recorded — which is the common case and not an
// error.
func (c *claudeWaker) reach(sessionID string) bool {
	c.found = c.lookup(sessionID)
	return c.found != nil
}

// lookup finds the record for sessionID.
//
// Looking up by session id rather than pid is what makes pid reuse harmless: a
// recycled pid belongs to a record naming a different session, so it never
// matches. Records are removed when their process exits, and one left behind by
// a crash fails later at connect.
func (c *claudeWaker) lookup(sessionID string) *claudeSession {
	if c.dir == "" || sessionID == "" {
		return nil
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !claudeRecordName.MatchString(e.Name()) {
			continue
		}
		rec, err := readClaudeRecord(filepath.Join(c.dir, e.Name()))
		if err != nil || rec.SessionID != sessionID {
			continue
		}
		if rec.MessagingSocketPath == "" || rec.PeerProtocol != claudePeerProtocol || rec.Pid <= 0 {
			continue
		}
		return &claudeSession{pid: rec.Pid, sock: rec.MessagingSocketPath}
	}
	return nil
}

func readClaudeRecord(path string) (claudeRecord, error) {
	var rec claudeRecord
	f, err := os.Open(path)
	if err != nil {
		return rec, err
	}
	defer f.Close()
	dec := json.NewDecoder(&limitedReader{r: f, left: claudeRecordCap})
	if err := dec.Decode(&rec); err != nil {
		return rec, err
	}
	return rec, nil
}

// poke connects, proves the endpoint is who the record said, authenticates, and
// writes one line.
//
// The order matters. Connecting first is what makes a missing key mean "the
// host's key layout changed" rather than "this session is still starting up":
// the socket, the key and the record are three separate files written at three
// different moments, so their absence is only meaningful once a live peer has
// answered.
func (c *claudeWaker) poke(ctx context.Context, sessionID, body string) error {
	sess := c.found
	if sess == nil {
		if sess = c.lookup(sessionID); sess == nil {
			return fmt.Errorf("no messaging inbox recorded for session %s", sessionID)
		}
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", sess.sock)
	if err != nil {
		return err
	}
	defer conn.Close()

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("unexpected connection type %T for %s", conn, sess.sock)
	}
	if deadline, has := ctx.Deadline(); has {
		if err := unixConn.SetDeadline(deadline); err != nil {
			return err
		}
	}

	if pid, known := peerPID(unixConn); known && pid != sess.pid {
		return fmt.Errorf("endpoint at %s is pid %d, want %d — refusing to write", sess.sock, pid, sess.pid)
	}

	token, err := c.peerToken(sess)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}

	frame, err := claudeFrame(token, body)
	if err != nil {
		return err
	}
	if _, err := unixConn.Write(frame); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		select {
		case <-ctx.Done():
		case <-time.After(darwinLinger):
		}
	}
	return nil
}

// peerToken reads the token this session's listener will accept.
//
// The filename ties the token to one socket path, so a key left behind by a
// previous session cannot be presented to a different one.
func (c *claudeWaker) peerToken(sess *claudeSession) (string, error) {
	abs, err := filepath.Abs(sess.sock)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(abs))
	path := filepath.Join(c.dir, fmt.Sprintf("%d.%x.key", sess.pid, sum))

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	var key struct {
		PeerToken string `json:"peerToken"`
	}
	if err := json.Unmarshal(raw, &key); err != nil {
		return "", fmt.Errorf("parsing %s: %w", filepath.Base(path), err)
	}
	if key.PeerToken == "" {
		return "", fmt.Errorf("%s carries no peerToken", filepath.Base(path))
	}
	return key.PeerToken, nil
}

// claudeFrame builds the two newline-delimited lines the listener expects. The
// auth line must come first; anything else on an unauthenticated connection is
// dropped.
//
// `priority` is the host's own field, not something pager invented for it.
// Read out of the 2.1.241 binary, it is declared `optional()` over
// ["now","next","later"] roughly 2KB from `crossSessionInbound` — the same
// module as the rest of this protocol. "now" is what a poke means: the point
// of waking a session is that it stops waiting for its next turn.
//
// What the host does with the value is unmeasured, and this code does not
// depend on the answer. The field being optional is the only property that
// matters — a host that ignores it, or one that drops it in a later version,
// leaves the poke exactly as effective as it would be without the field.
func claudeFrame(token, body string) ([]byte, error) {
	auth, err := json.Marshal(map[string]string{"type": "auth", "token": token})
	if err != nil {
		return nil, err
	}
	user, err := json.Marshal(map[string]any{
		"type":     "user",
		"priority": "now",
		"message":  map[string]string{"role": "user", "content": body},
	})
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	out.Write(auth)
	out.WriteByte('\n')
	out.Write(user)
	out.WriteByte('\n')
	if out.Len() > claudeLineCap {
		return nil, fmt.Errorf("poke frame is %d bytes, over the %d byte line cap", out.Len(), claudeLineCap)
	}
	return []byte(out.String()), nil
}

// limitedReader caps a decode without turning a long file into an EOF that
// looks like malformed JSON.
type limitedReader struct {
	r    interface{ Read([]byte) (int, error) }
	left int
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, fmt.Errorf("session record exceeds %d bytes", claudeRecordCap)
	}
	if len(p) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= n
	return n, err
}
