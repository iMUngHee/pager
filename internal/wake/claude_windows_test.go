package wake

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
