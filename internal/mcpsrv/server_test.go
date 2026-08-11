package mcpsrv

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unghee/pager/internal/clock"
	"github.com/unghee/pager/internal/deliver"
	"github.com/unghee/pager/internal/store"
)

// session drives the server the way a host does: newline-delimited JSON-RPC
// over a pipe pair. Anything less would test the handlers rather than the
// protocol surface an agent actually reaches.
type session struct {
	t   *testing.T
	in  *io.PipeWriter
	out *bufio.Reader
}

func start(t *testing.T, st *store.Store) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = New(st).Serve(ctx, inR, outW) }()
	t.Cleanup(func() {
		cancel()
		inW.Close()
	})

	s := &session{t: t, in: inW, out: bufio.NewReader(outR)}
	s.call(1, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	})
	s.notify("notifications/initialized")
	return s
}

func (s *session) write(msg map[string]any) {
	s.t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		s.t.Fatalf("encode request: %v", err)
	}
	if _, err := s.in.Write(append(raw, '\n')); err != nil {
		s.t.Fatalf("write request: %v", err)
	}
}

func (s *session) notify(method string) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": method})
}

// call sends a request and returns the matching response, skipping anything
// the server volunteers in between.
func (s *session) call(id int, method string, params map[string]any) map[string]any {
	s.t.Helper()
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		line, err := s.out.ReadBytes('\n')
		if err != nil {
			s.t.Fatalf("read response: %v", err)
		}
		var msg map[string]any
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // not a frame we care about
		}
		if got, ok := msg["id"].(float64); ok && int(got) == id {
			return msg
		}
	}
	s.t.Fatalf("no response to %s within the deadline", method)
	return nil
}

// text pulls the human-readable payload out of a tools/call result.
func text(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result: %v", resp)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("result has no content: %v", result)
	}
	var sb strings.Builder
	for _, item := range content {
		if m, ok := item.(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				sb.WriteString(s)
			}
		}
	}
	return sb.String()
}

func isError(t *testing.T, resp map[string]any) bool {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return true
	}
	flag, _ := result["isError"].(bool)
	return flag
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "msg.db"), clock.System{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seed(t *testing.T, st *store.Store, id, alias string) {
	t.Helper()
	if err := st.RecordSession(t.Context(), store.SessionRecord{
		ID: id, Tool: "claude", Root: "/tmp/mcp-workspace",
	}); err != nil {
		t.Fatalf("record %s: %v", id, err)
	}
	if ok, err := deliver.SetAlias(t.Context(), st, alias, id); err != nil || !ok {
		t.Fatalf("alias %s: ok=%v err=%v", alias, ok, err)
	}
}

// --- tests -------------------------------------------------------------

func TestToolsAreAdvertised(t *testing.T) {
	st := newStore(t)
	s := start(t, st)

	resp := s.call(2, "tools/list", map[string]any{})
	raw, _ := json.Marshal(resp)
	for _, want := range []string{"msg_send", "msg_list", "target", "body"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("tools/list does not advertise %q:\n%s", want, raw)
		}
	}
}

// TestSendRecordsTheCallersSession is the criterion that matters: MCP hands the
// server no identity, so the sender on the stored row has to come from the
// shared resolver rather than from anything the caller said.
func TestSendRecordsTheCallersSession(t *testing.T) {
	st := newStore(t)
	seed(t, st, "sender-session", "sender-box")
	seed(t, st, "recipient-session", "recipient-box")
	t.Setenv("PAGER_SESSION", "sender-session")

	s := start(t, st)
	resp := s.call(2, "tools/call", map[string]any{
		"name":      "msg_send",
		"arguments": map[string]any{"target": "recipient-box", "body": "over MCP"},
	})
	if isError(t, resp) {
		t.Fatalf("msg_send failed: %s", text(t, resp))
	}
	if got := text(t, resp); !strings.Contains(got, "recipient-box") {
		t.Errorf("result does not name the target: %s", got)
	}

	var sender, label, body string
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT sender_session, sender_label, body FROM messages ORDER BY id DESC LIMIT 1").
		Scan(&sender, &label, &body); err != nil {
		t.Fatalf("read the stored message: %v", err)
	}
	if sender != "sender-session" {
		t.Errorf("sender_session = %q, want %q", sender, "sender-session")
	}
	if label != "sender-box" {
		t.Errorf("sender_label = %q, want the sender's own alias", label)
	}
	if body != "over MCP" {
		t.Errorf("body = %q", body)
	}
}

func TestSendRefusedWithoutASession(t *testing.T) {
	st := newStore(t)
	seed(t, st, "recipient-session", "recipient-box")
	// No PAGER_SESSION, and no host to detect.
	t.Setenv("PAGER_SESSION", "")
	t.Setenv("PAGER_CLIENT", "none")

	s := start(t, st)
	resp := s.call(2, "tools/call", map[string]any{
		"name":      "msg_send",
		"arguments": map[string]any{"target": "recipient-box", "body": "who am I"},
	})
	if !isError(t, resp) {
		t.Fatal("an unattributed msg_send succeeded, want it refused")
	}

	var queued int
	if err := st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&queued); err != nil {
		t.Fatalf("count: %v", err)
	}
	if queued != 0 {
		t.Errorf("%d messages were queued by a refused send, want 0", queued)
	}
}

func TestSendReportsAnAmbiguousTarget(t *testing.T) {
	st := newStore(t)
	seed(t, st, "sender-session", "sender-box")
	seed(t, st, "r1", "review-one")
	if ok, err := deliver.SetAlias(t.Context(), st, "review-two", "r1"); err != nil || !ok {
		t.Fatalf("second alias: ok=%v err=%v", ok, err)
	}
	t.Setenv("PAGER_SESSION", "sender-session")

	s := start(t, st)
	resp := s.call(2, "tools/call", map[string]any{
		"name":      "msg_send",
		"arguments": map[string]any{"target": "review", "body": "which one?"},
	})
	if !isError(t, resp) {
		t.Fatal("an ambiguous target succeeded")
	}
	got := text(t, resp)
	if !strings.Contains(got, "review-one") || !strings.Contains(got, "review-two") {
		t.Errorf("the error does not list the candidates: %s", got)
	}
}

func TestListShowsWhatIsWaiting(t *testing.T) {
	st := newStore(t)
	seed(t, st, "me", "my-box")
	t.Setenv("PAGER_SESSION", "me")
	if _, err := deliver.Send(t.Context(), st, deliver.SendRequest{
		Alias: "my-box", Body: "read me", Label: "@someone", Human: true,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	s := start(t, st)
	resp := s.call(2, "tools/call", map[string]any{
		"name": "msg_list", "arguments": map[string]any{},
	})
	if isError(t, resp) {
		t.Fatalf("msg_list failed: %s", text(t, resp))
	}
	got := text(t, resp)
	for _, want := range []string{"read me", "@someone", "waiting"} {
		if !strings.Contains(got, want) {
			t.Errorf("msg_list output is missing %q:\n%s", want, got)
		}
	}
}

func TestSendRejectsEmptyArguments(t *testing.T) {
	st := newStore(t)
	seed(t, st, "me", "my-box")
	t.Setenv("PAGER_SESSION", "me")

	s := start(t, st)
	resp := s.call(2, "tools/call", map[string]any{
		"name": "msg_send", "arguments": map[string]any{"target": "my-box", "body": "   "},
	})
	if !isError(t, resp) {
		t.Error("an empty body was accepted")
	}
}
