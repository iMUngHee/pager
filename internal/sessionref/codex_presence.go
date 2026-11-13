package sessionref

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// CodexThreadStates observes loaded threads on the existing managed daemon.
// It never starts a server or loads a thread. Missing entries are unknown;
// an error can accompany useful partial observations.
func CodexThreadStates(ctx context.Context) (map[string]string, error) {
	states := make(map[string]string)
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return states, fmt.Errorf("find Codex home: %w", err)
		}
		home = filepath.Join(userHome, ".codex")
	}
	socket := filepath.Join(home, "app-server-control", "app-server-control.sock")
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	defer transport.CloseIdleConnections()
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		return states, fmt.Errorf("connect to existing Codex daemon: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	rpc := codexPresenceRPC{conn: conn}
	if _, err := rpc.call(ctx, "initialize", struct {
		ClientInfo   map[string]string `json:"clientInfo"`
		Capabilities map[string]bool   `json:"capabilities"`
	}{map[string]string{"name": "pager_presence", "version": "1"}, map[string]bool{"experimentalApi": true}}); err != nil {
		return states, err
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"method":"initialized","params":{}}`)); err != nil {
		return states, fmt.Errorf("initialize Codex presence: %w", err)
	}
	var cursor *string
	seenCursors := make(map[string]bool)
	seenThreads := make(map[string]bool)
	var failures []error
	for page := 0; page < 64; page++ {
		raw, err := rpc.call(ctx, "thread/loaded/list", struct {
			Cursor *string `json:"cursor,omitempty"`
			Limit  int     `json:"limit"`
		}{cursor, 100})
		if err != nil {
			return states, errors.Join(append(failures, err)...)
		}
		var result struct {
			Data       *[]string `json:"data"`
			NextCursor *string   `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return states, errors.Join(append(failures, fmt.Errorf("decode Codex loaded threads: %w", err))...)
		}
		if result.Data == nil {
			return states, errors.Join(append(failures, errors.New("codex loaded threads omitted data"))...)
		}
		for _, id := range *result.Data {
			if strings.TrimSpace(id) == "" {
				failures = append(failures, errors.New("codex loaded thread has an empty id"))
				continue
			}
			if seenThreads[id] {
				continue
			}
			seenThreads[id] = true
			raw, err := rpc.call(ctx, "thread/read", struct {
				ThreadID     string `json:"threadId"`
				IncludeTurns bool   `json:"includeTurns"`
			}{id, false})
			if err != nil {
				failures = append(failures, err)
				if ctx.Err() != nil {
					return states, errors.Join(failures...)
				}
				continue
			}
			var read struct {
				Thread struct {
					ID     string `json:"id"`
					Status struct {
						Type string `json:"type"`
					} `json:"status"`
				} `json:"thread"`
			}
			if err := json.Unmarshal(raw, &read); err != nil {
				failures = append(failures, fmt.Errorf("decode Codex thread %s: %w", id, err))
			} else if read.Thread.ID != id || strings.TrimSpace(read.Thread.Status.Type) == "" {
				failures = append(failures, fmt.Errorf("invalid Codex thread identity or status for %s", id))
			} else {
				states[id] = read.Thread.Status.Type
			}
		}
		cursor = result.NextCursor
		if cursor == nil {
			return states, errors.Join(failures...)
		}
		if seenCursors[*cursor] {
			return states, errors.Join(append(failures, errors.New("codex loaded threads repeated a cursor"))...)
		}
		seenCursors[*cursor] = true
	}
	return states, errors.Join(append(failures, errors.New("codex loaded threads exceeded page limit"))...)
}

type codexPresenceRPC struct {
	conn *websocket.Conn
	id   int
}

// Each method supplies its own wire parameters and validates its raw result.
func (c *codexPresenceRPC) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.id++
	request, err := json.Marshal(struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{c.id, method, params})
	if err != nil {
		return nil, err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, request); err != nil {
		return nil, fmt.Errorf("write Codex %s: %w", method, err)
	}
	for {
		kind, raw, err := c.conn.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("read Codex %s: %w", method, err)
		}
		if kind != websocket.MessageText {
			return nil, errors.New("codex presence received a non-text message")
		}
		var response struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, fmt.Errorf("decode Codex %s response: %w", method, err)
		}
		if response.Method != "" || string(response.ID) != strconv.Itoa(c.id) {
			continue
		}
		if response.Error != nil {
			return nil, fmt.Errorf("codex %s: %s (%d)", method, response.Error.Message, response.Error.Code)
		}
		if len(response.Result) == 0 || string(response.Result) == "null" {
			return nil, fmt.Errorf("codex %s omitted its result", method)
		}
		return response.Result, nil
	}
}
