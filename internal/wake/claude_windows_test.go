package wake

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func TestPokeWindowsNamedPipe(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\LOCAL\pager-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	ln, err := winio.ListenPipe(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	frames := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(Deadline))
		scanner := bufio.NewScanner(conn)
		var lines []string
		for len(lines) < 2 && scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		frames <- lines
	}()
	r := newRegistry(t)
	r.record(t, os.Getpid(), "pipe-session", path, claudePeerProtocol)
	r.key(t, os.Getpid(), path, "test-token")
	w := r.waker()
	ctx, cancel := context.WithTimeout(context.Background(), Deadline)
	defer cancel()
	if err := w.poke(ctx, "pipe-session", "test-body"); err != nil {
		t.Fatalf("poke: %v", err)
	}
	select {
	case lines := <-frames:
		if len(lines) != 2 {
			t.Fatalf("got %d frames, want 2", len(lines))
		}
		var auth struct{ Type, Token string }
		var message struct {
			Type, Priority string
			Message        struct{ Role, Content string }
		}
		if err := json.Unmarshal([]byte(lines[0]), &auth); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(lines[1]), &message); err != nil {
			t.Fatal(err)
		}
		if auth.Type != "auth" || auth.Token != "test-token" {
			t.Fatalf("unexpected auth: %+v", auth)
		}
		if message.Type != "user" || message.Priority != "now" || message.Message.Role != "user" || message.Message.Content != "test-body" {
			t.Fatalf("unexpected message: %+v", message)
		}
	case <-ctx.Done():
		t.Fatal("named pipe did not receive frames")
	}
}

func TestWindowsNamedPipeMissingKeyIsRefused(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\LOCAL\pager-no-key-%d-%d`, os.Getpid(), time.Now().UnixNano())
	ln, err := winio.ListenPipe(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
	}()
	r := newRegistry(t)
	r.record(t, os.Getpid(), "pipe-session", path, claudePeerProtocol)
	ctx, cancel := context.WithTimeout(context.Background(), Deadline)
	defer cancel()
	err = r.waker().poke(ctx, "pipe-session", "must not be sent")
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("got %v, want ErrRefused", err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("accept did not finish")
	}
}

func TestWindowsNamedPipeServerPIDIsThisProcess(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\LOCAL\pager-server-pid-%d-%d`, os.Getpid(), time.Now().UnixNano())
	ln, err := winio.ListenPipe(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if conn, err := ln.Accept(); err == nil {
			conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), Deadline)
	defer cancel()
	conn, err := dialClaude(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	pid, known := pipeServerPID(conn)
	if !known || pid != os.Getpid() {
		t.Fatalf("got pid %d known %v, want %d", pid, known, os.Getpid())
	}
}

func TestWindowsNamedPipeWrongServerIsRefused(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\LOCAL\pager-wrong-pid-%d-%d`, os.Getpid(), time.Now().UnixNano())
	ln, err := winio.ListenPipe(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	received := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(Deadline))
		got, _ := io.ReadAll(conn)
		received <- got
	}()
	// The record names another live process, so this process serving the
	// pipe is exactly the squatter the check exists for. The key is present
	// so only the pid check can explain a refusal.
	other := os.Getppid()
	r := newRegistry(t)
	r.record(t, other, "pipe-session", path, claudePeerProtocol)
	r.key(t, other, path, "test-token")
	ctx, cancel := context.WithTimeout(context.Background(), Deadline)
	defer cancel()
	err = r.waker().poke(ctx, "pipe-session", "must not be sent")
	want := fmt.Sprintf("is pid %d, want %d — refusing to write", os.Getpid(), other)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want an error containing %q", err, want)
	}
	select {
	case got := <-received:
		if len(got) != 0 {
			t.Fatalf("server received %q, want nothing", got)
		}
	case <-ctx.Done():
		t.Fatal("server did not see the connection close")
	}
}

func TestWindowsNamedPipeCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := dialClaude(ctx, `\\.\pipe\LOCAL\pager-canceled-test`)
	if conn != nil {
		conn.Close()
		t.Fatal("canceled dial returned a connection")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
