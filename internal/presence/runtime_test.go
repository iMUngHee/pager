package presence

import (
	"context"
	"errors"
	"testing"
)

func TestProbeKeepsPartialPositivesAndOneSnapshot(t *testing.T) {
	calls := 0
	probe := newProbe(func(int, int64) (bool, bool) { return false, false }, func(context.Context) (map[string]string, error) {
		calls++
		return map[string]string{"loaded": "idle"}, errors.New("later page failed")
	})
	if got := probe(t.Context(), Session{ID: "claude", Tool: "claude"}); got.Thread != "unknown" || calls != 0 {
		t.Fatalf("Claude unexpectedly queried Codex: %+v calls=%d", got, calls)
	}
	for _, id := range []string{"loaded", "missing", "loaded"} {
		got := probe(t.Context(), Session{ID: id, Tool: "codex"})
		want := "unknown"
		if id == "loaded" {
			want = "idle"
		}
		if got.Thread != want {
			t.Fatalf("%s: %+v", id, got)
		}
	}
	if calls != 1 {
		t.Fatalf("snapshot calls=%d", calls)
	}
}
