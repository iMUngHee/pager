package presence

import (
	"context"
	"sync"

	"github.com/iMUngHee/pager/internal/sessionref"
)

// NewProbe takes one lazy Codex snapshot per operation, rather than one RPC
// connection per historical session. Failure preserves positive observations
// already returned; an absent thread remains unknown.
func NewProbe() Probe {
	return newProbe(sessionref.AliveAt, sessionref.CodexThreadStates)
}

func newProbe(host func(int, int64) (bool, bool), threads func(context.Context) (map[string]string, error)) Probe {
	var once sync.Once
	var states map[string]string
	return func(ctx context.Context, s Session) Evidence {
		e := Evidence{Host: "unknown", Thread: "unknown"}
		alive, known := host(s.HostPID, s.HostStart)
		if known {
			if alive {
				e.Host = "live"
			} else {
				e.Host = "gone"
			}
		}
		if s.Tool == "codex" {
			// Partial observations remain useful; failure leaves every missing ID unknown.
			once.Do(func() { states, _ = threads(ctx) })
			if state := states[s.ID]; state != "" {
				e.Thread = state
			}
		}
		return e
	}
}
