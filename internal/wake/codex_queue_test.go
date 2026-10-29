package wake

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/iMUngHee/pager/internal/clock"
	"github.com/iMUngHee/pager/internal/deliver"
	"github.com/iMUngHee/pager/internal/store"
)

// Re-execute a copy of the Go test binary as codex, so the same subprocess
// boundary works on Windows and Unix without touching a real client or inbox.
func TestMain(m *testing.M) {
	if path := os.Getenv("PAGER_TEST_QUEUE_DB"); path != "" {
		db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
		if err != nil {
			panic(err)
		}
		defer db.Close()
		add := func(thread, body string) int64 {
			payload, _ := json.Marshal([]map[string]string{{"type": "text", "text": body}})
			res, err := db.Exec("INSERT INTO fixture_queue(thread,input) VALUES(?,?)", thread, string(payload))
			if err != nil {
				panic(err)
			}
			id, _ := res.LastInsertId()
			return id
		}
		if len(os.Args) > 1 && os.Args[1] == "queue" {
			add(os.Args[3], os.Args[5]) // old unconditional CLI path
			return
		}
		dec, enc := json.NewDecoder(bufio.NewReader(os.Stdin)), json.NewEncoder(os.Stdout)
		for {
			var req struct {
				ID     any    `json:"id"`
				Method string `json:"method"`
				Params struct {
					Thread     string `json:"threadId"`
					Submission string `json:"queuedSubmissionId"`
					Input      []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"input"`
				} `json:"params"`
			}
			if err := dec.Decode(&req); err != nil {
				return
			}
			if req.ID == nil {
				continue
			}
			var result any = map[string]any{}
			switch req.Method {
			case "thread/queue/list":
				if os.Getenv("PAGER_TEST_BAD_QUEUE_LIST") != "" {
					enc.Encode(map[string]any{"id": req.ID, "error": map[string]any{"code": -32601, "message": "unsupported queue protocol"}})
					continue
				}
				rows, err := db.Query("SELECT id,input FROM fixture_queue WHERE thread=? ORDER BY id", req.Params.Thread)
				if err != nil {
					panic(err)
				}
				data := []map[string]any{}
				for rows.Next() {
					var id int64
					var raw string
					rows.Scan(&id, &raw)
					var input any
					json.Unmarshal([]byte(raw), &input)
					data = append(data, map[string]any{"id": strconv.FormatInt(id, 10), "input": input})
				}
				rows.Close()
				result = map[string]any{"data": data, "nextCursor": nil}
			case "thread/queue/add":
				add(req.Params.Thread, req.Params.Input[0].Text)
			case "thread/queue/delete":
				if _, err := db.Exec("DELETE FROM fixture_queue WHERE thread=? AND id=?", req.Params.Thread, req.Params.Submission); err != nil {
					panic(err)
				}
			}
			if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}); err != nil {
				return
			}
		}
	}
	os.Exit(m.Run())
}

func nativeQueueFixture(t *testing.T) *sql.DB {
	t.Helper()
	root := filepath.Join(t.TempDir(), "codex home")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "queue-fixture.sqlite")
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("CREATE TABLE fixture_queue(id INTEGER PRIMARY KEY,thread TEXT,input TEXT)"); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME", root)
	t.Setenv("PAGER_TEST_QUEUE_DB", path)
	return db
}

func queueCount(t *testing.T, db *sql.DB, thread string) int {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM fixture_queue WHERE thread=?", thread).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCodexQueueCoalescesAndRearms(t *testing.T) {
	db := nativeQueueFixture(t)
	body := PokeBody("box", "sender", "codex")
	for i := 0; i < 4; i++ {
		if err := runCodexQueue(t.Context(), "t1", body); err != nil && !strings.Contains(err.Error(), "already queued") {
			t.Fatal(err)
		}
	}
	if n := queueCount(t, db, "t1"); n != 1 {
		t.Fatalf("pending notifications=%d, want 1", n)
	}
	if _, err := db.Exec("DELETE FROM fixture_queue WHERE thread='t1'"); err != nil {
		t.Fatal(err)
	}
	if err := runCodexQueue(t.Context(), "t1", body); err != nil {
		t.Fatal(err)
	}
	if n := queueCount(t, db, "t1"); n != 1 {
		t.Fatalf("after consumption=%d, want 1", n)
	}
	if err := runCodexQueue(t.Context(), "t2", body); err != nil {
		t.Fatal(err)
	}
	if n := queueCount(t, db, "t2"); n != 1 {
		t.Fatalf("other thread=%d, want 1", n)
	}
}

func TestCodexQueueConcurrentProcessesKeepOneWake(t *testing.T) {
	db := nativeQueueFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runCodexQueue(context.Background(), "busy-thread", PokeBody("box", "sender", "claude")); err != nil && !strings.Contains(err.Error(), "already queued") {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := queueCount(t, db, "busy-thread"); n != 1 {
		t.Fatalf("concurrent pending notifications=%d, want 1", n)
	}
}

func TestCodexQueueRepairsDuplicatesAndPreservesUserInput(t *testing.T) {
	db := nativeQueueFixture(t)
	body := PokeBody("box", "sender", "claude")
	for _, text := range []string{body, body, body, "Explain " + body} {
		raw, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
		if _, err := db.Exec("INSERT INTO fixture_queue(thread,input) VALUES('t1',?)", string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err := runCodexQueue(t.Context(), "t1", body); err != nil && !strings.Contains(err.Error(), "already queued") {
		t.Fatal(err)
	}
	if n := queueCount(t, db, "t1"); n != 2 {
		t.Fatalf("queue entries=%d, want 1 wake + 1 user input", n)
	}
}

func TestCodexQueueUnsupportedProtocolDoesNotEnqueueBlindly(t *testing.T) {
	db := nativeQueueFixture(t)
	t.Setenv("PAGER_TEST_BAD_QUEUE_LIST", "1")
	if err := runCodexQueue(t.Context(), "t1", PokeBody("box", "sender", "codex")); err == nil {
		t.Fatal("unsupported protocol succeeded")
	}
	if n := queueCount(t, db, "t1"); n != 0 {
		t.Fatalf("unsupported protocol still queued %d notifications", n)
	}
}

func TestCodexQueueCleanupRechecksUnreadMailAndPreservesUserInput(t *testing.T) {
	db := nativeQueueFixture(t)
	t.Setenv("PAGER_WAKE", "on")
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "mail.db"), clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.RecordSession(t.Context(), store.SessionRecord{ID: "t1", Tool: "codex"}); err != nil {
		t.Fatal(err)
	}
	if _, err := deliver.SetAlias(t.Context(), st, "box", "t1"); err != nil {
		t.Fatal(err)
	}
	mail, err := deliver.Send(t.Context(), st, deliver.SendRequest{Alias: "box", Body: "new mail arrived before cleanup", Human: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := runCodexQueue(t.Context(), "t1", PokeBody("box", "sender", "claude")); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal([]map[string]string{{"type": "text", "text": "Keep my queued input"}})
	if _, err := db.Exec("INSERT INTO fixture_queue(thread,input) VALUES('t1',?)", string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := ClearCodexQueue(t.Context(), st, "t1"); err != nil {
		t.Fatal(err)
	}
	if n := queueCount(t, db, "t1"); n != 2 {
		t.Fatalf("cleanup removed the new mail's wake: count=%d", n)
	}
	if _, err := deliver.MarkListed(t.Context(), st, []int64{mail.ID}); err != nil {
		t.Fatal(err)
	}
	if err := ClearCodexQueue(t.Context(), st, "t1"); err != nil {
		t.Fatal(err)
	}
	if n := queueCount(t, db, "t1"); n != 1 {
		t.Fatalf("after read: queue count=%d, want only user input", n)
	}
	var kept string
	if err := db.QueryRow("SELECT input FROM fixture_queue WHERE thread='t1'").Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != string(raw) {
		t.Fatalf("user input changed: %s", kept)
	}
}
