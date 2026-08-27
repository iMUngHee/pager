package deliver

import (
	"context"
	"fmt"

	"github.com/unghee/pager/internal/store"
)

// Inbox is one alias and what is waiting in it, as `pager inbox` shows it.
//
// The host fields are reported rather than judged. Whether the process behind
// an inbox is still running is a question only the machine the session runs on
// can answer, and answering it means reading process state — which this package
// does not do. A caller pairs these with sessionref.Alive and renders a word.
type Inbox struct {
	Alias string
	// Waiting is how many messages nobody has dealt with: neither injected by
	// a hook nor pulled up by an agent looking at its own inbox. It is the same
	// count `pager ls --waiting` would report for this alias, by construction —
	// both use undealtWith.
	Waiting int
	// SessionID is the alias's current holder, or "" when nothing holds it.
	SessionID string
	// HostPid and HostStart identify the holder's host process. Both are zero
	// when there is no holder, or when host detection failed at attach time —
	// which is not the same as the process being gone, and must not be rendered
	// as if it were.
	HostPid   int
	HostStart int64
}

// Inboxes reports every alias with mail waiting in it, alphabetically.
//
// This is the whole-store view that per-session listing cannot give: List is
// scoped to one session, who counts nothing, and export dumps delivered mail
// too, so answering "who is waiting on mail right now" used to mean either one
// call per session — too expensive at a one-second refresh — or reading the
// tables directly, which is what the tmux badge in ~/.config resorted to.
//
// Aliases with nothing waiting are left out. The question is who is waiting,
// and a row of zeroes is not an answer to it — a caller wanting the full roster
// wants sessions, which is Roster's job.
//
// The order is alphabetical rather than by pressure because the intended
// consumer redraws every second: sorting by count would make the display
// reshuffle under the reader whenever a message lands.
//
// The join is left outer on purpose. An alias outlives its session — that is
// what makes claim possible at all — and mail sitting in an inbox whose holder
// is gone is exactly the case worth surfacing, not the one to drop.
func Inboxes(ctx context.Context, st *store.Store) ([]Inbox, error) {
	rows, err := st.DB().QueryContext(ctx, `
		SELECT m.alias, COUNT(m.id),
		       COALESCE(a.session_id, ''), COALESCE(s.host_pid, 0), COALESCE(s.host_start, 0)
		  FROM messages m
		  JOIN aliases a ON a.alias = m.alias
		  LEFT JOIN sessions s ON s.session_id = a.session_id
		 WHERE `+undealtWith+`
		 GROUP BY m.alias
		 ORDER BY m.alias`)
	if err != nil {
		return nil, fmt.Errorf("list inboxes: %w", err)
	}
	defer rows.Close()

	var out []Inbox
	for rows.Next() {
		var i Inbox
		if err := rows.Scan(&i.Alias, &i.Waiting, &i.SessionID, &i.HostPid, &i.HostStart); err != nil {
			return nil, fmt.Errorf("list inboxes: %w", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
