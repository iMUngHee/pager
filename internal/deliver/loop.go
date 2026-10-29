package deliver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Metadata carries cooperative work signals. Unknown activity is deliberately
// different from idle: missing telemetry never establishes a stalled task.
type Metadata struct {
	Task     string `json:"task,omitempty"`
	Kind     string `json:"event_kind,omitempty"`
	Activity string `json:"activity,omitempty"`
	Progress string `json:"progress,omitempty"`
	EventID  string `json:"event_id,omitempty"`
}

// Normalized is shared by local sends and relay imports, so event semantics
// and metadata bounds do not depend on which transport supplied the message.
func (m Metadata) Normalized() (Metadata, error) {
	if m.Kind == "" {
		m.Kind = "update"
	}
	if m.Activity == "" {
		m.Activity = "unknown"
	}
	switch m.Kind {
	case "update", "result", "failure", "release":
	default:
		return m, fmt.Errorf("unknown event kind %q", m.Kind)
	}
	switch m.Activity {
	case "unknown", "idle", "working":
	default:
		return m, fmt.Errorf("unknown activity %q", m.Activity)
	}
	for _, v := range []struct {
		name, value string
		limit       int
	}{{"task", m.Task, 128}, {"event_id", m.EventID, 256}, {"progress", m.Progress, 2048}} {
		if !utf8.ValidString(v.value) || len(v.value) > v.limit {
			return m, fmt.Errorf("invalid %s (maximum %d UTF-8 bytes)", v.name, v.limit)
		}
	}
	return m, nil
}

// checkBreaker is a wake decision, not a send quota. It reuses the sender's
// store transaction, after INSERT, and never discards an accepted message.
// Bodies alone are insufficient: both peers must affirm idle with fresh
// signals and the same exchange/progress/epochs must persist across time.
func checkBreaker(ctx context.Context, c *sql.Conn, now int64, req SendRequest, recipient string) (string, error) {
	if req.Sender == "" || recipient == "" || req.Peer != "" || req.Sender == recipient {
		return "", nil
	}
	a, b := req.Sender, recipient
	if a > b {
		a, b = b, a
	}
	reset := func() (string, error) {
		_, err := c.ExecContext(ctx, "DELETE FROM loop_guard WHERE a=? AND b=? AND task=?", a, b, req.Task)
		return "", err
	}
	if req.Kind != "update" || req.Activity != "idle" {
		return reset()
	}
	idle, ttl := 2*time.Minute, 2*time.Minute
	for name, dst := range map[string]*time.Duration{"PAGER_LOOP_IDLE": &idle, "PAGER_LOOP_SIGNAL_TTL": &ttl} {
		if raw := os.Getenv(name); raw != "" {
			v, err := time.ParseDuration(raw)
			if err != nil || v < time.Millisecond {
				return "", fmt.Errorf("%s must be at least 1ms; wake remains enabled", name)
			}
			*dst = v
		}
	}
	var peerBody, peerProgress, peerKind, peerActivity string
	var at int64
	err := c.QueryRowContext(ctx, `SELECT body,progress,event_kind,activity,created_at FROM messages
	  WHERE sender_session=? AND recipient_session=? AND task=?
	    AND (delivered_at IS NOT NULL OR listed_at IS NOT NULL)
	  ORDER BY id DESC LIMIT 1`, recipient, req.Sender, req.Task).Scan(&peerBody, &peerProgress, &peerKind, &peerActivity, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return reset()
	}
	if err != nil {
		return "", err
	}
	if peerKind != "update" || peerActivity != "idle" || at > now || now-at > ttl.Milliseconds() {
		return reset()
	}
	var epochA, epochB int64
	if err := c.QueryRowContext(ctx, "SELECT causal_epoch FROM sessions WHERE session_id=?", a).Scan(&epochA); err != nil {
		return "", err
	}
	if err := c.QueryRowContext(ctx, "SELECT causal_epoch FROM sessions WHERE session_id=?", b).Scan(&epochB); err != nil {
		return "", err
	}
	bodyA, bodyB, progressA, progressB := req.Body, peerBody, req.Progress, peerProgress
	if req.Sender != a {
		bodyA, bodyB, progressA, progressB = peerBody, req.Body, peerProgress, req.Progress
	}
	raw, _ := json.Marshal([]any{bodyA, bodyB, progressA, progressB, epochA, epochB})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(raw))
	var prior string
	var since int64
	err = c.QueryRowContext(ctx, "SELECT fingerprint,unchanged_since FROM loop_guard WHERE a=? AND b=? AND task=?", a, b, req.Task).Scan(&prior, &since)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if prior != fingerprint || since > now {
		since = now
	}
	if _, err := c.ExecContext(ctx, `INSERT INTO loop_guard(a,b,task,fingerprint,unchanged_since,updated_at) VALUES(?,?,?,?,?,?)
	 ON CONFLICT(a,b,task) DO UPDATE SET fingerprint=excluded.fingerprint,unchanged_since=excluded.unchanged_since,updated_at=excluded.updated_at`, a, b, req.Task, fingerprint, since, now); err != nil {
		return "", err
	}
	if now-since < idle.Milliseconds() {
		return "", nil
	}
	return fmt.Sprintf("repeated exchange with both peers reporting idle and unchanged progress for %s; mail is stored; automatic wake paused (read it normally or resume with real work/new information)", time.Duration(now-since)*time.Millisecond), nil
}

// IsPagerMetadata reports whether metadata needs the semantic relay extension.
func (m Metadata) IsPagerMetadata() bool {
	return m.Task != "" || m.EventID != "" || m.Progress != "" || (m.Kind != "" && m.Kind != "update") || (m.Activity != "" && m.Activity != "unknown")
}

// PauseSummary is shared by send receipts and mailbox displays.
func PauseSummary(note string) string { return "note: " + strings.TrimSpace(note) }
