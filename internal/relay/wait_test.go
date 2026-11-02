package relay

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/iMUngHee/pager/internal/deliver"
)

// testTick stands in for wait's one-second tick, so these tests take ticks of
// a quarter second instead.
const testTick = 250 * time.Millisecond

// waitRun is one call of wait running in the background.
type waitRun struct {
	out  bytes.Buffer
	done chan error
}

func startWait(ctx context.Context, n node, peer string, deadline time.Time) *waitRun {
	r := &waitRun{done: make(chan error, 1)}
	go func() { r.done <- wait(ctx, n.st, peer, deadline, testTick, &r.out) }()
	return r
}

// result waits at most within for wait to return, and gives its output and
// exit code. Reading out is safe once done has delivered.
func (r *waitRun) result(t *testing.T, within time.Duration) (string, int) {
	t.Helper()
	select {
	case err := <-r.done:
		return r.out.String(), exitCode(t, err)
	case <-time.After(within):
		t.Fatalf("wait did not return within %v", within)
		return "", -1
	}
}

// running fails the test if wait has already returned.
func (r *waitRun) running(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.done:
		t.Fatalf("wait returned early (%v) with %q", err, r.out.String())
	default:
	}
}

func TestWait(t *testing.T) {
	far := func() time.Time { return time.Now().Add(40 * testTick) }

	t.Run("mail already waiting gives ready at once", func(t *testing.T) {
		a, _ := pair(t)
		session(t, a, "sa", "sape")
		sendRemote(t, a, "sa", "kipa", "waiting")
		// A deadline already past: the look comes first, and mail found by it
		// is the answer however late the wait began. The peer is named the
		// way relay out takes it, upper case folded.
		start := time.Now()
		got, code := startWait(t.Context(), a, "VM", start.Add(-time.Second)).result(t, testTick)
		if code != 0 || got != "ready\n" {
			t.Fatalf("wait = %q, exit %d; want \"ready\\n\", exit 0", got, code)
		}
		if took := time.Since(start); took >= testTick {
			t.Errorf("ready took %v, want it before the first tick (%v)", took, testTick)
		}
	})

	t.Run("mail sent during the wait gives ready within two ticks", func(t *testing.T) {
		a, _ := pair(t)
		session(t, a, "sa", "sape")
		r := startWait(t.Context(), a, "vm", far())
		time.Sleep(3 * testTick / 2)
		r.running(t)
		sendRemote(t, a, "sa", "kipa", "sent during the wait")
		sent := time.Now()
		got, code := r.result(t, 4*testTick)
		if code != 0 || got != "ready\n" {
			t.Fatalf("wait = %q, exit %d; want \"ready\\n\", exit 0", got, code)
		}
		if took := time.Since(sent); took > 2*testTick {
			t.Errorf("ready came %v after the send, want within two ticks (%v)", took, 2*testTick)
		}
	})

	t.Run("mail whose final result was acked does not count", func(t *testing.T) {
		a, b := pair(t)
		session(t, a, "sa", "sape")
		session(t, b, "sb", "kipa")
		sendRemote(t, a, "sa", "kipa", "carried")
		results, code := in(t, b, "mac", out(t, a))
		if code != 0 || len(results) != 1 || results[0].Status != StatusImported {
			t.Fatalf("relay in = %+v, exit %d", results, code)
		}
		if code := ack(t, a, encode(t, results[0])); code != 0 {
			t.Fatalf("relay ack exit %d", code)
		}
		if n := count(t, a, "SELECT count(*) FROM messages WHERE dst_peer = 'vm' AND relayed_at IS NOT NULL"); n != 1 {
			t.Fatalf("%d messages closed by the ack, want 1", n)
		}
		got, code := startWait(t.Context(), a, "vm", time.Now().Add(testTick)).result(t, 4*testTick)
		if code != 0 || got != "timeout\n" {
			t.Fatalf("wait = %q, exit %d; want \"timeout\\n\", exit 0", got, code)
		}
	})

	// Mail queued for another peer is not this peer's to carry.
	t.Run("mail for another peer does not count", func(t *testing.T) {
		a, _ := pair(t)
		c := newNode(t)
		if _, err := deliver.AddPeer(t.Context(), a.st, "vm2", c.id); err != nil {
			t.Fatalf("a adds vm2: %v", err)
		}
		session(t, a, "sa", "sape")
		if _, err := deliver.Send(t.Context(), a.st, deliver.SendRequest{
			Alias: "kipa@vm2", Peer: "vm2", Body: "elsewhere", Sender: "sa", Label: "sape",
		}); err != nil {
			t.Fatalf("send to kipa@vm2: %v", err)
		}
		got, code := startWait(t.Context(), a, "vm", time.Now().Add(testTick)).result(t, 4*testTick)
		if code != 0 || got != "timeout\n" {
			t.Fatalf("wait for vm = %q, exit %d; want \"timeout\\n\", exit 0", got, code)
		}
	})

	t.Run("no mail gives timeout at the deadline", func(t *testing.T) {
		a, _ := pair(t)
		// Not a whole number of ticks, so a wait that slept a full tick past
		// the deadline would answer late.
		deadline := time.Now().Add(2*testTick + testTick/5)
		got, code := startWait(t.Context(), a, "vm", deadline).result(t, 6*testTick)
		ended := time.Now()
		if code != 0 || got != "timeout\n" {
			t.Fatalf("wait = %q, exit %d; want \"timeout\\n\", exit 0", got, code)
		}
		if ended.Before(deadline) {
			t.Errorf("timeout %v before the deadline", deadline.Sub(ended))
		}
		if late := ended.Sub(deadline); late > testTick/2 {
			t.Errorf("timeout %v after the deadline, want within half a tick", late)
		}
	})

	t.Run("an unregistered peer exits 2", func(t *testing.T) {
		a, _ := pair(t)
		got, code := startWait(t.Context(), a, "nosuch", far()).result(t, testTick)
		if code != 2 || got != "" {
			t.Fatalf("wait = %q, exit %d; want no output, exit 2", got, code)
		}
	})

	t.Run("a store that cannot be read exits 1", func(t *testing.T) {
		a, _ := pair(t)
		if err := a.st.Close(); err != nil {
			t.Fatal(err)
		}
		got, code := startWait(t.Context(), a, "vm", far()).result(t, testTick)
		if code != 1 || got != "" {
			t.Fatalf("wait = %q, exit %d; want no output, exit 1", got, code)
		}
	})

	t.Run("an answer that cannot be written exits 1", func(t *testing.T) {
		a, _ := pair(t)
		session(t, a, "sa", "sape")
		sendRemote(t, a, "sa", "kipa", "waiting")
		err := wait(t.Context(), a.st, "vm", far(), testTick, failingWriter{})
		if code := exitCode(t, err); code != 1 {
			t.Fatalf("wait with a closed stdout: exit %d (%v), want 1", code, err)
		}
	})

	t.Run("a peer removed during the wait exits 2", func(t *testing.T) {
		a, _ := pair(t)
		r := startWait(t.Context(), a, "vm", far())
		time.Sleep(3 * testTick / 2)
		r.running(t)
		if err := deliver.RemovePeer(t.Context(), a.st, "vm"); err != nil {
			t.Fatalf("remove vm: %v", err)
		}
		got, code := r.result(t, 4*testTick)
		if code != 2 || got != "" {
			t.Fatalf("wait = %q, exit %d; want no output, exit 2", got, code)
		}
	})

	t.Run("ctx ending gives no output and a nil error", func(t *testing.T) {
		a, _ := pair(t)
		ctx, cancel := context.WithCancel(t.Context())
		r := startWait(ctx, a, "vm", far())
		// A quarter tick into a sleep, so a wait that noticed only at its
		// next look would be three quarters of a tick late.
		time.Sleep(5 * testTick / 4)
		r.running(t)
		cancel()
		select {
		case err := <-r.done:
			if err != nil || r.out.Len() != 0 {
				t.Fatalf("wait = %q, %v; want no output and a nil error", r.out.String(), err)
			}
		case <-time.After(testTick / 4):
			t.Fatal("wait went on after its context ended")
		}

		// Ended before the first look, as when stdin is already at end of
		// file: still nothing printed and no error.
		got, code := startWait(ctx, a, "vm", far()).result(t, testTick)
		if code != 0 || got != "" {
			t.Fatalf("wait on an ended context = %q, exit %d; want no output, exit 0", got, code)
		}
	})
}

// failingWriter is a stdout whose reader has gone.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestWaitQueryUsesOutboundIndex: the look wait repeats every second is one
// indexed lookup, never a scan of the message table, however much mail the
// store keeps.
func TestWaitQueryUsesOutboundIndex(t *testing.T) {
	a, _ := pair(t)
	rows, err := a.st.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+waitQuery, "vm")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan: %v", err)
	}
	all := strings.Join(plan, "\n")
	if !strings.Contains(all, "messages_outbound") {
		t.Errorf("plan does not use messages_outbound:\n%s", all)
	}
	if strings.Contains(all, "SCAN messages") {
		t.Errorf("plan scans messages:\n%s", all)
	}
}
