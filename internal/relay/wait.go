package relay

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/iMUngHee/pager/internal/deliver"
	"github.com/iMUngHee/pager/internal/store"
)

// Limits on how long relay wait holds on, and how often it looks.
const (
	DefaultWait = 5 * time.Minute
	MinWait     = time.Second
	MaxWait     = time.Hour
	waitTick    = time.Second
)

// waitQuery answers, in one statement, whether peer is registered and whether
// mail for it is waiting to leave. The second half asks for the rows relay out
// would emit, and is answered from the same partial index.
const waitQuery = `SELECT EXISTS(SELECT 1 FROM peers WHERE name = ?1),
       EXISTS(SELECT 1 FROM messages WHERE dst_peer = ?1 AND relayed_at IS NULL)`

// Wait prints "ready" once mail for peer is waiting to leave, or "timeout"
// when deadline passes first. It is advisory: relay out decides what is sent,
// and mail that is only deferred on the other side still counts as waiting.
// When ctx ends first (the transport hung up) it prints nothing.
//
// Each look is one statement that completes before the sleep, so a long wait
// holds no transaction and no connection between looks.
func Wait(ctx context.Context, st *store.Store, peer string, deadline time.Time, w io.Writer) error {
	return wait(ctx, st, peer, deadline, waitTick, w)
}

func wait(ctx context.Context, st *store.Store, peer string, deadline time.Time, tick time.Duration, w io.Writer) error {
	name, err := deliver.NormalizePeerName(peer)
	if err != nil {
		return &ExitError{Code: 2, Err: err}
	}
	for {
		var known, waiting bool
		if err := st.DB().QueryRowContext(ctx, waitQuery, name).Scan(&known, &waiting); err != nil {
			if ctx.Err() != nil {
				return nil // a look cut short by the hang-up, or begun after it
			}
			return &ExitError{Code: 1, Err: fmt.Errorf("read outbound mail: %w", err)}
		}
		if !known {
			return exitf(2, "%w %q (pager peer ls)", deliver.ErrUnknownPeer, name)
		}
		answer := "ready"
		if !waiting {
			left := time.Until(deadline)
			if left > 0 {
				t := time.NewTimer(min(tick, left))
				select {
				case <-ctx.Done():
					t.Stop()
					return nil
				case <-t.C:
				}
				continue
			}
			answer = "timeout"
		}
		if _, err := fmt.Fprintln(w, answer); err != nil {
			return &ExitError{Code: 1, Err: fmt.Errorf("write answer: %w", err)}
		}
		return nil
	}
}
