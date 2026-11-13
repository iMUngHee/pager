// Package presence separates hook recency from evidence about a session's runtime.
package presence

import (
	"context"
	"time"
)

// Session is the recorded identity needed for a presence observation.
type Session struct {
	ID, Tool  string
	Heartbeat int64
	HostPID   int
	HostStart int64
}

// Evidence contains independent observations, not a conclusion about the session.
// Empty values mean unknown; a shared host's existence does not identify a thread.
type Evidence struct{ Host, Thread string }

// Probe observes a session without changing its recorded identity.
type Probe func(context.Context, Session) Evidence

// State keeps presence separate from the last hook's age.
type State struct {
	Evidence
	Recent, Available, Gone bool
}

// Classify treats active/idle as positive thread evidence. A Codex host may be
// shared or replaced before the next hook, so neither host answer proves its
// individual thread's presence. Claude records a dedicated process identity.
func Classify(s Session, evidence Evidence, now int64, stale time.Duration) State {
	if evidence.Host == "" {
		evidence.Host = "unknown"
	}
	if evidence.Thread == "" {
		evidence.Thread = "unknown"
	}
	state := State{Evidence: evidence, Recent: s.Heartbeat >= now-stale.Milliseconds()}
	if s.Tool == "codex" {
		state.Available = evidence.Thread == "active" || evidence.Thread == "idle"
	} else if s.Tool == "claude" {
		state.Available = evidence.Host == "live"
		state.Gone = evidence.Host == "gone"
	}
	return state
}

// Visible retains recent diagnostic rows and older sessions positively observed.
// A hidden old unknown session remains addressable and is not reclaimable.
func (s State) Visible() bool { return s.Recent || s.Available }
