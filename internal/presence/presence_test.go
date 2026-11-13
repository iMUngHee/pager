package presence

import (
	"testing"
	"time"
)

func TestClassificationSeparatesRecencyHostAndThread(t *testing.T) {
	const now int64 = 1_000_000_000
	stale := 12 * time.Hour
	old := now - int64((26*time.Hour+31*time.Minute)/time.Millisecond)
	for _, tc := range []struct {
		name, tool               string
		heartbeat                int64
		evidence                 Evidence
		visible, available, gone bool
	}{
		{"old idle without host", "codex", old, Evidence{Thread: "idle"}, true, true, false},
		{"old active after host key moved", "codex", old, Evidence{Thread: "active", Host: "gone"}, true, true, false},
		{"shared live daemon proves no individual thread", "codex", old, Evidence{Host: "live"}, false, false, false},
		{"shared dead daemon proves no individual thread", "codex", old, Evidence{Host: "gone"}, false, false, false},
		{"unloaded is not dead", "codex", old, Evidence{Thread: "notLoaded"}, false, false, false},
		{"system error is not dead", "codex", old, Evidence{Thread: "systemError"}, false, false, false},
		{"old unknown is not dead", "claude", old, Evidence{}, false, false, false},
		{"dedicated live survives age", "claude", old, Evidence{Host: "live"}, true, true, false},
		{"dedicated gone", "claude", old, Evidence{Host: "gone"}, false, false, true},
		{"recent unknown remains visible", "codex", now, Evidence{}, true, false, false},
		{"recent gone remains diagnostic", "claude", now, Evidence{Host: "gone"}, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(Session{Tool: tc.tool, Heartbeat: tc.heartbeat}, tc.evidence, now, stale)
			if got.Visible() != tc.visible || got.Available != tc.available || got.Gone != tc.gone {
				t.Fatalf("state=%+v visible=%v; want visible=%v available=%v gone=%v", got, got.Visible(), tc.visible, tc.available, tc.gone)
			}
			if got.Thread == "" || got.Host == "" {
				t.Fatalf("missing explicit unknown observations: %+v", got)
			}
		})
	}
}

func TestRecencyBoundary(t *testing.T) {
	const now int64 = 1_000_000_000
	for _, delta := range []int64{0, 1} {
		s := Classify(Session{Heartbeat: now - (12 * time.Hour).Milliseconds() - delta}, Evidence{}, now, 12*time.Hour)
		if s.Recent != (delta == 0) {
			t.Fatalf("delta=%d recent=%v", delta, s.Recent)
		}
	}
}
