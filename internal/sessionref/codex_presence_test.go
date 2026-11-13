package sessionref

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type codexTestRequest struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Unix socket paths must also fit macOS's short address limit.
func codexFixtureHome(t *testing.T) string {
	t.Helper()
	base := "/tmp"
	if runtime.GOOS == "windows" {
		base = ""
	} // Windows uses its native temp directory.
	home, err := os.MkdirTemp(base, "pgc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return home
}

// Fixtures isolate the runtime endpoint from the user's daemon.
func serveCodexPresence(t *testing.T, respond func(codexTestRequest) any, homes ...string) string {
	t.Helper()
	var home string
	if len(homes) > 0 {
		home = homes[0]
	} else {
		home = codexFixtureHome(t)
	}
	dir := filepath.Join(home, "app-server-control")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "app-server-control.sock"))
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Unix-socket fixture unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	var workers sync.WaitGroup
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workers.Add(1)
		defer workers.Done()
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept presence client: %v", err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var request codexTestRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				t.Errorf("decode presence request: %v", err)
				return
			}
			var response any
			switch request.Method {
			case "initialized":
				continue
			case "initialize":
				response = map[string]string{"userAgent": "fixture"}
			case "thread/loaded/list", "thread/read":
				response = respond(request)
			default:
				t.Errorf("unexpected method %q: only read-only presence calls are allowed", request.Method)
				return
			}
			if response == nil {
				continue
			}
			if raw, ok := response.([]byte); ok {
				for _, frame := range bytes.Split(raw, []byte("\n")) {
					if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
						return
					}
				}
				continue
			}
			raw, err = json.Marshal(map[string]any{"id": request.ID, "result": response})
			if err != nil {
				t.Errorf("encode fixture response: %v", err)
				return
			}
			if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
				return
			}
		}
	})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
		workers.Wait()
	})
	return home
}

func threadReply(t *testing.T, request codexTestRequest, status string) any {
	t.Helper()
	var params struct {
		ID           string `json:"threadId"`
		IncludeTurns *bool  `json:"includeTurns"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Errorf("decode read params: %v", err)
	}
	if params.IncludeTurns == nil || *params.IncludeTurns {
		t.Error("presence lookup must explicitly exclude turn history")
	}
	return map[string]any{"thread": map[string]any{"id": params.ID, "status": map[string]string{"type": status}}}
}

func TestCodexPresencePagesAndRuntimeStates(t *testing.T) {
	serveCodexPresence(t, func(request codexTestRequest) any {
		if request.Method == "thread/read" {
			var params struct {
				ID string `json:"threadId"`
			}
			_ = json.Unmarshal(request.Params, &params)
			return threadReply(t, request, map[string]string{"working": "active", "quiet": "idle", "unloaded": "notLoaded", "broken": "systemError", "future": "newState"}[params.ID])
		}
		var params struct {
			Cursor *string `json:"cursor"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Error(err)
		}
		if params.Cursor == nil {
			return map[string]any{"data": []string{"working", "quiet"}, "nextCursor": "page-two"}
		}
		if *params.Cursor != "page-two" {
			t.Errorf("cursor = %q, want page-two", *params.Cursor)
		}
		return map[string]any{"data": []string{"unloaded", "broken", "future"}, "nextCursor": nil}
	})
	states, err := CodexThreadStates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"working": "active", "quiet": "idle", "unloaded": "notLoaded", "broken": "systemError", "future": "newState"}
	if !reflect.DeepEqual(states, want) {
		t.Errorf("states = %v, want %v", states, want)
	}
	if _, ok := states["not-in-loaded-list"]; ok {
		t.Error("a missing thread must remain unknown")
	}
}

func TestCodexPresencePreservesPartialResults(t *testing.T) {
	for _, bad := range []string{"page-error", "malformed-page", "missing-data", "null-data", "repeated-cursor", "read-error", "bad-id", "missing-status", "blank-status", "wrong-status-shape", "bad-json", "null-result"} {
		t.Run(bad, func(t *testing.T) {
			serveCodexPresence(t, func(request codexTestRequest) any {
				if request.Method == "thread/loaded/list" {
					var params struct {
						Cursor *string `json:"cursor"`
					}
					_ = json.Unmarshal(request.Params, &params)
					if params.Cursor == nil {
						return map[string]any{"data": []string{"good"}, "nextCursor": "next"}
					}
					switch bad {
					case "page-error":
						return []byte(fmt.Sprintf(`{"id":%d,"error":{"code":-1,"message":"offline"}}`, request.ID))
					case "malformed-page":
						return map[string]any{"data": 42}
					case "missing-data":
						return map[string]any{}
					case "null-data":
						return map[string]any{"data": nil}
					case "repeated-cursor":
						return map[string]any{"data": []string{}, "nextCursor": "next"}
					}
					return map[string]any{"data": []string{"bad", "later-good"}}
				}
				var params struct {
					ID string `json:"threadId"`
				}
				_ = json.Unmarshal(request.Params, &params)
				if params.ID != "bad" {
					return threadReply(t, request, "idle")
				}
				switch bad {
				case "read-error":
					return []byte(fmt.Sprintf(`{"id":%d,"error":{"code":-32600,"message":"thread not loaded"}}`, request.ID))
				case "bad-id":
					return map[string]any{"thread": map[string]any{"id": "another-thread", "status": map[string]string{"type": "active"}}}
				case "missing-status":
					return map[string]any{"thread": map[string]string{"id": "bad"}}
				case "blank-status":
					return threadReply(t, request, " ")
				case "wrong-status-shape":
					return map[string]any{"thread": map[string]any{"id": "bad", "status": "idle"}}
				case "bad-json":
					return []byte("{")
				case "null-result":
					return []byte(fmt.Sprintf(`{"id":%d,"result":null}`, request.ID))
				}
				return nil
			})
			states, err := CodexThreadStates(t.Context())
			if err == nil || states["good"] != "idle" {
				t.Fatalf("states=%v err=%v, want prior positive plus error", states, err)
			}
			if _, ok := states["bad"]; ok {
				t.Errorf("failed read acquired a status: %v", states)
			}
			switch bad {
			case "read-error", "bad-id", "missing-status", "blank-status", "wrong-status-shape", "bad-json", "null-result":
				if states["later-good"] != "idle" {
					t.Errorf("read failure hid later positive observation: %v", states)
				}
			}
		})
	}
}

func TestCodexPresenceIgnoresUnrelatedResponses(t *testing.T) {
	serveCodexPresence(t, func(request codexTestRequest) any {
		if request.Method == "thread/loaded/list" {
			return []byte(fmt.Sprintf(`{"method":"thread/status/changed","params":{}}
{"id":9999,"result":{"data":["wrong"]}}
{"id":%d,"result":{"data":["right"]}}`, request.ID))
		}
		return threadReply(t, request, "active")
	})
	states, err := CodexThreadStates(t.Context())
	if err != nil || states["right"] != "active" || len(states) != 1 {
		t.Fatalf("states=%v err=%v", states, err)
	}
}

func TestCodexPresenceDeduplicatesIDsAndRejectsBlank(t *testing.T) {
	var reads atomic.Int64
	serveCodexPresence(t, func(request codexTestRequest) any {
		if request.Method == "thread/loaded/list" {
			return map[string]any{"data": []string{"one", "one", " "}}
		}
		reads.Add(1)
		return threadReply(t, request, "idle")
	})
	states, err := CodexThreadStates(t.Context())
	if err == nil || states["one"] != "idle" || reads.Load() != 1 {
		t.Fatalf("states=%v err=%v reads=%d", states, err, reads.Load())
	}
}

func TestCodexPresenceBoundsPagination(t *testing.T) {
	var pages atomic.Int64
	serveCodexPresence(t, func(request codexTestRequest) any {
		return map[string]any{"data": []string{}, "nextCursor": fmt.Sprintf("page-%d", pages.Add(1))}
	})
	states, err := CodexThreadStates(t.Context())
	if err == nil || !strings.Contains(err.Error(), "page limit") || len(states) != 0 || pages.Load() != 64 {
		t.Fatalf("states=%v err=%v pages=%d", states, err, pages.Load())
	}
}

func TestCodexPresenceDeadlineKeepsPositive(t *testing.T) {
	serveCodexPresence(t, func(request codexTestRequest) any {
		if request.Method == "thread/loaded/list" {
			return map[string]any{"data": []string{"fast", "slow"}}
		}
		var params struct {
			ID string `json:"threadId"`
		}
		_ = json.Unmarshal(request.Params, &params)
		if params.ID == "fast" {
			return threadReply(t, request, "idle")
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	states, err := CodexThreadStates(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || states["fast"] != "idle" || time.Since(start) > time.Second {
		t.Fatalf("states=%v err=%v elapsed=%v", states, err, time.Since(start))
	}
}

func TestCodexPresenceOwnBudget(t *testing.T) {
	serveCodexPresence(t, func(codexTestRequest) any { return nil })
	start := time.Now()
	states, err := CodexThreadStates(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) || len(states) != 0 || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("states=%v err=%v elapsed=%v", states, err, time.Since(start))
	}
}

func TestCodexPresenceMissingSocketDoesNotCreateState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	states, err := CodexThreadStates(t.Context())
	if err == nil || len(states) != 0 {
		t.Fatalf("states=%v err=%v", states, err)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing daemon directory changed: entries=%v err=%v", entries, err)
	}
}

func TestCodexPresenceMissingHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	states, err := CodexThreadStates(t.Context())
	if err == nil || len(states) != 0 {
		t.Fatalf("states=%v err=%v, want unknown when no home is available", states, err)
	}
}

func TestCodexPresenceDefaultHome(t *testing.T) {
	userHome := codexFixtureHome(t)
	serveCodexPresence(t, func(request codexTestRequest) any {
		if request.Method == "thread/loaded/list" {
			return map[string]any{"data": []string{"one"}}
		}
		return threadReply(t, request, "idle")
	}, filepath.Join(userHome, ".codex"))
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)
	states, err := CodexThreadStates(t.Context())
	if err != nil || states["one"] != "idle" {
		t.Fatalf("states=%v err=%v", states, err)
	}
}

func TestCodexPresenceLiveDaemon(t *testing.T) {
	if os.Getenv("PAGER_TEST_CODEX_PRESENCE") != "1" {
		t.Skip("opt-in read-only check against the existing managed daemon")
	}
	states, err := CodexThreadStates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if id := os.Getenv("CODEX_THREAD_ID"); id != "" && states[id] != "active" && states[id] != "idle" {
		t.Fatalf("calling thread status = %q, want active or idle", states[id])
	}
	ids := make([]string, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t.Logf("%s %s", id, states[id])
	}
}
