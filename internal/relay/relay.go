// Package relay moves mail between two pager stores without knowing how.
//
// pager never reaches another machine. Something outside it — the transport —
// runs `pager relay out` against one store, carries the lines to the other,
// feeds them to `pager relay in` there, and carries the result lines back to
// `pager relay ack`. Everything pager promises holds for any transport that
// moves lines in order and may lose, repeat or cut them: ssh, a file, a queue.
//
// Each direction between two stores is a link with its own gap-free sequence.
// The receiving store keeps one high-water mark per link, which is what makes
// a repeated line a duplicate rather than a second copy, and a missing one a
// gap the receiver waits on rather than skips. The causal hop a message
// carried in its own store comes along, so a chain of agent replies is capped
// across stores exactly as it is within one.
package relay

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iMUngHee/pager/internal/deliver"
	"github.com/iMUngHee/pager/internal/store"
	"github.com/iMUngHee/pager/internal/wake"
)

// Version is the highest protocol version this binary reads and the one it
// writes. A field that changes what a receiver must do is a new version, even
// when it is only added; purely descriptive additions are not.
const Version = 1

// Kinds of line. "roster" is reserved for a later version.
const (
	kindMsg    = "msg"
	kindResult = "result"
)

// Result statuses. The first three are final: the sender may stop resending.
const (
	StatusImported  = "imported"
	StatusDuplicate = "duplicate"
	StatusRejected  = "rejected"
	StatusDeferred  = "deferred"
	StatusRefused   = "refused"
)

// Limits on what relay out emits per run.
const (
	DefaultLimit = 100
	MaxLimit     = 1000
)

// Inbound cap: imports accepted from one peer per window. It bounds what
// another store can push in, now that imports are kept out of the local human
// budget. The value is an estimate, like the breaker's, and hitting it only
// delays mail — the run stops and the transport retries later.
const (
	inboundWindow  = time.Hour
	maxInboundPeer = 60
)

// maxFrom bounds a reply address taken from another store.
const maxFrom = 128

// maxTool bounds the sender's host name, which is display text in the poke.
const maxTool = 16

// maxLiveNames bounds the alias names a "no such alias" rejection lists.
const maxLiveNames = 10

// relayTime renders timestamps the way the archive does: RFC 3339 in UTC with
// exactly three fractional digits.
const relayTime = "2006-01-02T15:04:05.000Z07:00"

// Link identifies one line's place in a link.
type Link struct {
	Store string `json:"store"`
	ID    string `json:"id"`
	Seq   int64  `json:"seq"`
}

// Ident is a message's original identity: the store that first wrote it and
// its id there.
type Ident struct {
	Store string `json:"store"`
	ID    int64  `json:"id"`
}

// Record is one message on its way to another store.
type Record struct {
	V        int     `json:"v"`
	Kind     string  `json:"kind"`
	Link     Link    `json:"link"`
	Origin   Ident   `json:"origin"`
	DstStore string  `json:"dst_store"`
	To       string  `json:"to"`
	From     *string `json:"from"`
	Label    string  `json:"label"`
	Tool     string  `json:"tool,omitempty"`
	Hop      int     `json:"hop"`
	SentAt   string  `json:"sent_at"`
	Body     string  `json:"body"`
	// Via is reserved for forwarding. A version 1 receiver rejects a record
	// that carries it rather than importing mail it cannot route correctly.
	Via json.RawMessage `json:"via,omitempty"`
}

// Result is a receiving store's answer to one record.
type Result struct {
	V            int    `json:"v"`
	Kind         string `json:"kind"`
	Store        string `json:"store"`
	MaxV         int    `json:"max_v"`
	Link         Link   `json:"link"`
	OriginID     int64  `json:"origin_id"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
}

// ExitError carries the process exit status a relay command ends with.
//
// 1 is a database or I/O failure, 2 a protocol or configuration problem that
// needs a person (a refused stream, a usage error), 3 input that ended inside
// a line — the transport was cut and may simply retry.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

func exitf(code int, format string, args ...any) error {
	return &ExitError{Code: code, Err: fmt.Errorf(format, args...)}
}

// peerRow is the registration a relay command runs against.
type peerRow struct {
	name, storeID, linkID string
}

func readPeer(ctx context.Context, c *sql.Conn, name string) (peerRow, error) {
	p := peerRow{name: name}
	err := c.QueryRowContext(ctx,
		"SELECT store_id, link_id FROM peers WHERE name = ?", name).Scan(&p.storeID, &p.linkID)
	if errors.Is(err, sql.ErrNoRows) {
		return peerRow{}, exitf(2, "%w %q (pager peer ls)", deliver.ErrUnknownPeer, name)
	}
	if err != nil {
		return peerRow{}, fmt.Errorf("read peer %q: %w", name, err)
	}
	return p, nil
}

// heldAlias is the name a session is best known by, read on c — the same
// ordering as deliver.PrimaryAlias, inside the caller's transaction.
func heldAlias(ctx context.Context, c *sql.Conn, session string) (string, error) {
	var alias string
	err := c.QueryRowContext(ctx,
		"SELECT alias FROM aliases WHERE session_id = ? ORDER BY updated_at DESC, alias LIMIT 1",
		session).Scan(&alias)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return alias, err
}

func sessionTool(ctx context.Context, c *sql.Conn, session string) (string, error) {
	var tool string
	err := c.QueryRowContext(ctx, "SELECT tool FROM sessions WHERE session_id = ?", session).Scan(&tool)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return tool, err
}

// Out writes the records waiting to leave for peer, in sequence order.
//
// It is a pure read: nothing is marked when a line is written, so until a
// final result is acknowledged every run emits the same pending records in the
// same order. Everything a record says is read inside one transaction and only
// written out after it ends, so a concurrent rebind or new-id is seen entirely
// before or entirely after, and no output happens under the write lock.
func Out(ctx context.Context, st *store.Store, peer string, limit int, w io.Writer) error {
	name, err := deliver.NormalizePeerName(peer)
	if err != nil {
		return &ExitError{Code: 2, Err: err}
	}
	if limit < 1 || limit > MaxLimit {
		return exitf(2, "--limit must be between 1 and %d (got %d)", MaxLimit, limit)
	}

	var records []Record
	err = st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		records = records[:0]
		p, err := readPeer(ctx, c, name)
		if err != nil {
			return err
		}
		own, err := store.IDOn(ctx, c)
		if err != nil {
			return err
		}

		rows, err := c.QueryContext(ctx, `
			SELECT id, alias, COALESCE(sender_session, ''), sender_label, body, hop, created_at, dst_link, dst_seq
			  FROM messages
			 WHERE dst_peer = ? AND relayed_at IS NULL
			 ORDER BY dst_seq
			 LIMIT ?`, name, limit)
		if err != nil {
			return fmt.Errorf("read outbound mail: %w", err)
		}
		type row struct {
			rec    Record
			sender string
		}
		var pending []row
		for rows.Next() {
			var r row
			var alias string
			var created int64
			if err := rows.Scan(&r.rec.Origin.ID, &alias, &r.sender, &r.rec.Label, &r.rec.Body,
				&r.rec.Hop, &created, &r.rec.Link.ID, &r.rec.Link.Seq); err != nil {
				rows.Close()
				return fmt.Errorf("read outbound mail: %w", err)
			}
			r.rec.V, r.rec.Kind = Version, kindMsg
			r.rec.Link.Store, r.rec.Origin.Store, r.rec.DstStore = own, own, p.storeID
			r.rec.To = strings.TrimSuffix(alias, "@"+name)
			r.rec.SentAt = time.UnixMilli(created).UTC().Format(relayTime)
			pending = append(pending, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read outbound mail: %w", err)
		}

		// The reply address is the name the sender holds now, else its
		// session id, which the sending store can still resolve. A send with
		// no session has no reply address at all.
		for _, r := range pending {
			if r.sender != "" {
				from, err := heldAlias(ctx, c, r.sender)
				if err != nil {
					return fmt.Errorf("read sender alias: %w", err)
				}
				if from == "" {
					from = r.sender
				}
				// A name the receiver would drop anyway is not sent, which also
				// keeps an overlong alias from swelling the record.
				if len(from) <= maxFrom {
					r.rec.From = &from
				}
				tool, err := sessionTool(ctx, c, r.sender)
				if err != nil {
					return fmt.Errorf("read sender tool: %w", err)
				}
				// Bounded like every other field, so no record can outgrow
				// the reader's line bound: a host name longer than the
				// receiver keeps is not sent.
				if len(tool) <= maxTool {
					r.rec.Tool = tool
				}
			}
			records = append(records, r.rec)
		}
		return nil
	})
	if err != nil {
		return err
	}

	for _, rec := range records {
		if err := writeLine(w, rec); err != nil {
			return &ExitError{Code: 1, Err: fmt.Errorf("write record: %w", err)}
		}
	}
	return nil
}

// recipient is a session to poke once a run's commits are done.
type recipient struct {
	session, alias, label, tool string
}

// pokeAll wakes each distinct recipient once and reports on diag.
func pokeAll(ctx context.Context, rs []recipient, diag io.Writer) {
	seen := map[string]bool{}
	for _, r := range rs {
		if r.session == "" || seen[r.session] {
			continue
		}
		seen[r.session] = true
		if line := wake.Describe(wake.Wake(ctx, r.session, wake.PokeBody(r.alias, r.label, r.tool)), r.alias); line != "" {
			fmt.Fprintln(diag, line)
		}
	}
}

// In imports records from peer and answers each one on out.
//
// Every record is decided and committed on its own, so a run cut short keeps
// what it already took. Results are written after each commit, never under
// the write lock; the recipients are poked once, after the last commit,
// whichever way the run ends.
func In(ctx context.Context, st *store.Store, peer string, in io.Reader, out, diag io.Writer) error {
	name, err := deliver.NormalizePeerName(peer)
	if err != nil {
		return &ExitError{Code: 2, Err: err}
	}

	// Answered before reading anything, so a transport pointed at the wrong
	// peer name learns it even from an empty stream, and every refusal below
	// can say which store is answering.
	own, err := st.ID(ctx)
	if err != nil {
		return &ExitError{Code: 1, Err: err}
	}
	var known int
	if err := st.DB().QueryRowContext(ctx, "SELECT count(*) FROM peers WHERE name = ?", name).Scan(&known); err != nil {
		return &ExitError{Code: 1, Err: fmt.Errorf("read peer %q: %w", name, err)}
	}
	if known == 0 {
		return exitf(2, "%w %q (pager peer ls)", deliver.ErrUnknownPeer, name)
	}

	var woken []recipient
	defer func() { pokeAll(context.WithoutCancel(ctx), woken, diag) }()

	lr := newLineReader(in)
	for {
		if err := ctx.Err(); err != nil {
			return &ExitError{Code: 1, Err: fmt.Errorf("interrupted: %w", err)}
		}
		line, err := lr.next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case errors.Is(err, errTruncated):
			return &ExitError{Code: 3, Err: errors.New("input ended inside a record; it was not applied — resend")}
		case errors.Is(err, errLineTooLong):
			return refuse(out, Result{Store: own}, "malformed: "+err.Error())
		case err != nil:
			return &ExitError{Code: 1, Err: fmt.Errorf("read records: %w", err)}
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue // a blank line carries no record; a transport may add one
		}

		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return refuse(out, Result{Store: own}, "malformed: "+err.Error())
		}

		res, to, stop, err := importOne(ctx, st, name, rec)
		if err != nil {
			return err
		}
		if to.session != "" {
			woken = append(woken, to)
		}
		if err := writeLine(out, res); err != nil {
			return &ExitError{Code: 1, Err: fmt.Errorf("write result: %w", err)}
		}
		if res.Status == StatusRefused {
			return exitf(2, "refused: %s", res.Reason)
		}
		if stop {
			return nil
		}
	}
}

// refuse answers a line that cannot even be placed in a link, then stops.
func refuse(out io.Writer, res Result, reason string) error {
	res.V, res.Kind, res.MaxV = Version, kindResult, Version
	res.Status, res.Reason = StatusRefused, reason
	if err := writeLine(out, res); err != nil {
		return &ExitError{Code: 1, Err: fmt.Errorf("write result: %w", err)}
	}
	return exitf(2, "refused: %s", reason)
}

// importOne decides one record inside one write transaction, following the
// placement table and then the per-position table in the order written there:
// an earlier row always wins over a later one.
func importOne(ctx context.Context, st *store.Store, peer string, rec Record) (Result, recipient, bool, error) {
	var res Result
	var to recipient
	var stop bool
	err := st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		to, stop = recipient{}, false
		own, err := store.IDOn(ctx, c)
		if err != nil {
			return err
		}
		res = Result{V: Version, Kind: kindResult, Store: own, MaxV: Version, Link: rec.Link, OriginID: rec.Origin.ID}
		answer := func(status, reason string) error {
			res.Status, res.Reason = status, reason
			return nil
		}

		// Placement: can this record be put in a link at all?
		p, err := readPeer(ctx, c, peer)
		if err != nil {
			var ee *ExitError
			if errors.As(err, &ee) {
				return answer(StatusRefused, "unknown peer "+peer)
			}
			return err
		}
		switch {
		case rec.V < 1 || rec.Kind == "":
			return answer(StatusRefused, "malformed: missing version or kind")
		case rec.V > Version:
			return answer(StatusRefused, fmt.Sprintf("unsupported version %d (this store reads up to %d)", rec.V, Version))
		case rec.Kind != kindMsg:
			return answer(StatusRefused, "unsupported kind "+deliver.CleanReason(rec.Kind))
		// Both reasons name the id the record carries: after a `relay new-id`
		// on the sender, its link store is the id to rebind to.
		case rec.DstStore != own:
			return answer(StatusRefused, "wrong store: the record is for store "+deliver.CleanReason(rec.DstStore)+", and this is "+own)
		case rec.Link.Store != p.storeID:
			return answer(StatusRefused, "wrong store: the record comes from store "+deliver.CleanReason(rec.Link.Store)+", but peer "+peer+" is enrolled as "+p.storeID)
		case rec.Link.ID == "" || rec.Link.Seq < 1:
			return answer(StatusRefused, "malformed link")
		}

		var linkPeer, linkSrc, linkDst string
		var inSeq int64
		known := true
		err = c.QueryRowContext(ctx,
			"SELECT peer, src_store, dst_store, in_seq FROM peer_links WHERE link_id = ?",
			rec.Link.ID).Scan(&linkPeer, &linkSrc, &linkDst, &inSeq)
		if errors.Is(err, sql.ErrNoRows) {
			known = false
		} else if err != nil {
			return fmt.Errorf("read link: %w", err)
		}
		if known && (linkPeer != peer || linkSrc != rec.Link.Store || linkDst != rec.DstStore) {
			return answer(StatusRefused, "link mismatch: link "+deliver.CleanReason(rec.Link.ID)+" belongs to another pairing")
		}
		if !known && rec.Link.Seq != 1 {
			return answer(StatusRefused, fmt.Sprintf("unknown link %s starting at %d, not 1", deliver.CleanReason(rec.Link.ID), rec.Link.Seq))
		}

		originOK := rec.Origin.Store == rec.Link.Store && rec.Origin.ID > 0
		exists := func() (bool, error) {
			var n int
			err := c.QueryRowContext(ctx,
				"SELECT count(*) FROM messages WHERE src_store = ? AND src_id = ?",
				rec.Origin.Store, rec.Origin.ID).Scan(&n)
			return n > 0, err
		}
		advance := func() error {
			_, err := c.ExecContext(ctx, `
				INSERT INTO peer_links(link_id, peer, src_store, dst_store, in_seq) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(link_id) DO UPDATE SET in_seq = excluded.in_seq`,
				rec.Link.ID, peer, rec.Link.Store, rec.DstStore, rec.Link.Seq)
			if err != nil {
				return fmt.Errorf("advance link: %w", err)
			}
			return nil
		}

		// Already consumed: answer from what is stored, change nothing.
		if rec.Link.Seq <= inSeq {
			if !originOK {
				return answer(StatusRejected, "previously rejected")
			}
			found, err := exists()
			if err != nil {
				return fmt.Errorf("look up origin: %w", err)
			}
			if found {
				return answer(StatusDuplicate, "")
			}
			return answer(StatusRejected, "previously rejected")
		}
		if rec.Link.Seq > inSeq+1 {
			stop = true
			return answer(StatusDeferred, fmt.Sprintf("gap: expected %d, got %d", inSeq+1, rec.Link.Seq))
		}

		// The next record of the link. Content failures consume it.
		reject := func(reason string) error {
			if err := advance(); err != nil {
				return err
			}
			return answer(StatusRejected, reason)
		}
		switch {
		case !originOK:
			return reject("malformed origin")
		case len(rec.Via) > 0:
			return reject("forwarding unsupported")
		case rec.Hop < 0 || rec.Hop > deliver.MaxHop:
			return reject(fmt.Sprintf("hop %d out of range 0..%d", rec.Hop, deliver.MaxHop))
		case !utf8.ValidString(rec.Body) || len(rec.Body) > deliver.MaxRelayBody:
			return reject("body too large")
		}
		found, err := exists()
		if err != nil {
			return fmt.Errorf("look up origin: %w", err)
		}
		if found {
			if err := advance(); err != nil {
				return err
			}
			return answer(StatusDuplicate, "")
		}

		now := st.Now()
		since := now - inboundWindow.Milliseconds()
		var recent int
		var oldest sql.NullInt64
		if err := c.QueryRowContext(ctx,
			"SELECT count(*), min(created_at) FROM messages WHERE src_peer = ? AND created_at >= ?",
			peer, since).Scan(&recent, &oldest); err != nil {
			return fmt.Errorf("count imports: %w", err)
		}
		if recent >= maxInboundPeer {
			stop = true
			res.RetryAfterMS = max(oldest.Int64+inboundWindow.Milliseconds()-now, 1)
			return answer(StatusDeferred, fmt.Sprintf("rate: %d imports from %s in the last hour (limit %d)", recent, peer, maxInboundPeer))
		}

		var holder string
		target := strings.TrimSpace(rec.To)
		if target != "" && !strings.Contains(target, "@") {
			err := c.QueryRowContext(ctx,
				"SELECT COALESCE(session_id, '') FROM aliases WHERE alias = ?", target).Scan(&holder)
			if errors.Is(err, sql.ErrNoRows) {
				target = ""
			} else if err != nil {
				return fmt.Errorf("resolve %q: %w", rec.To, err)
			}
		} else {
			target = ""
		}
		if target == "" {
			names, err := deliver.LiveNames(ctx, c, now-store.DefaultStale.Milliseconds(), maxLiveNames)
			if err != nil {
				return err
			}
			reason := "no such alias " + deliver.CleanReason(rec.To)
			if len(names) > 0 {
				reason += "; live names here: " + strings.Join(names, ", ")
			}
			return reject(reason)
		}

		label := deliver.SanitizeLabel(rec.Label)
		if _, err := c.ExecContext(ctx, `
			INSERT INTO messages(alias, sender_session, sender_label, body, hop, origin, cause_id, created_at,
			                     src_peer, src_store, src_id, src_from)
			VALUES (?, NULL, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?)`,
			target, label, rec.Body, rec.Hop, deliver.OriginHuman, now,
			peer, rec.Origin.Store, rec.Origin.ID, normalizeFrom(rec.From)); err != nil {
			return fmt.Errorf("import message: %w", err)
		}
		if err := advance(); err != nil {
			return err
		}
		to = recipient{session: holder, alias: target, label: label + " on " + peer, tool: normalizeTool(rec.Tool)}
		return answer(StatusImported, "")
	})
	return res, to, stop, err
}

// normalizeFrom keeps a reply address only when it could be one: 1–128 bytes
// of UTF-8 with no whitespace, control character or '@'. Anything else is
// dropped rather than rejected — the message is still worth delivering, it
// just cannot be answered by name.
func normalizeFrom(from *string) any {
	if from == nil {
		return nil
	}
	f := *from
	if f == "" || len(f) > maxFrom || !utf8.ValidString(f) || strings.Contains(f, "@") {
		return nil
	}
	for _, r := range f {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return nil
		}
	}
	return f
}

// normalizeTool keeps a host name only when it looks like one; it is display
// text in the poke and nothing else.
func normalizeTool(tool string) string {
	if tool == "" || len(tool) > maxTool {
		return ""
	}
	for _, r := range tool {
		if r < 'a' || r > 'z' {
			return ""
		}
	}
	return tool
}

// Ack applies result lines from peer's store to the mail that left for it.
//
// The batch is validated whole before anything changes: a result from the
// wrong store, or in a version this binary cannot read, refuses all of it.
// Each line then matches at most one outbound row — by message id, link and
// sequence, and only while it has no final result — so a stale result from a
// link that has since been renumbered, or a result applied twice, changes
// nothing.
func Ack(ctx context.Context, st *store.Store, peer string, in io.Reader, diag io.Writer) error {
	name, err := deliver.NormalizePeerName(peer)
	if err != nil {
		return &ExitError{Code: 2, Err: err}
	}

	var results []Result
	truncated := false
	lr := newLineReader(in)
	for {
		line, err := lr.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, errTruncated) {
			truncated = true
			break
		}
		if errors.Is(err, errLineTooLong) {
			return exitf(2, "refused: %v", err)
		}
		if err != nil {
			return &ExitError{Code: 1, Err: fmt.Errorf("read results: %w", err)}
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r Result
		if err := json.Unmarshal(line, &r); err != nil {
			return exitf(2, "refused: malformed result: %v", err)
		}
		results = append(results, r)
	}

	var bounced []recipient
	var acked, ignored int
	err = st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		bounced, acked, ignored = bounced[:0], 0, 0
		p, err := readPeer(ctx, c, name)
		if err != nil {
			return err
		}
		own, err := store.IDOn(ctx, c)
		if err != nil {
			return err
		}
		for _, r := range results {
			switch {
			case r.Kind != kindResult:
				return exitf(2, "refused: not a result line (kind %q)", r.Kind)
			case r.V < 1 || r.V > Version:
				return exitf(2, "refused: unsupported result version %d", r.V)
			case r.Store != p.storeID:
				return exitf(2, "refused: results come from store %s, but %s is enrolled as %s", deliver.CleanReason(r.Store), name, p.storeID)
			}
		}

		now := st.Now()
		for _, r := range results {
			var final string
			switch r.Status {
			case StatusImported, StatusDuplicate:
				final = r.Status
			case StatusRejected:
				final = StatusRejected + ": " + deliver.CleanReason(r.Reason)
			default: // deferred, refused, or a status this binary does not know
				ignored++
				continue
			}
			if r.Link.Store != own {
				ignored++
				continue
			}
			res, err := c.ExecContext(ctx, `
				UPDATE messages SET relayed_at = ?, relay_result = ?
				 WHERE id = ? AND dst_peer = ? AND dst_link = ? AND dst_seq = ? AND relayed_at IS NULL`,
				now, final, r.OriginID, name, r.Link.ID, r.Link.Seq)
			if err != nil {
				return fmt.Errorf("apply result: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("apply result: %w", err)
			}
			if n == 0 {
				ignored++
				continue
			}
			acked++
			if r.Status == StatusRejected {
				to, err := bounce(ctx, c, now, r.OriginID, fmt.Sprintf("was not delivered: %s", deliver.CleanReason(r.Reason)))
				if err != nil {
					return err
				}
				if to.alias != "" {
					bounced = append(bounced, to)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(diag, "acked %d, bounced %d, ignored %d\n", acked, len(bounced), ignored)
	pokeAll(context.WithoutCancel(ctx), bounced, diag) // committed: tell them even if interrupted
	if truncated {
		return &ExitError{Code: 3, Err: errors.New("input ended inside a result line; the complete lines were applied")}
	}
	return nil
}

// bounce tells the sender of outbound message id what became of it, on c.
//
// The recipient is a name the sending session holds as this transaction
// commits. A send with no session, or a session that holds no name any more,
// gets no bounce — the outcome stays on the row, in its relay_result, which
// export shows. Like any local mail, the bounce follows its alias: if that
// name passes to another session before it is delivered, the new holder reads
// it.
//
// It is caused by the message it reports on and carries that message's hop,
// so it can never be used to restart a chain: a reply to a bounce sits one
// deeper than the message that bounced.
func bounce(ctx context.Context, c *sql.Conn, now, id int64, what string) (recipient, error) {
	var session, alias string
	var hop int
	if err := c.QueryRowContext(ctx,
		"SELECT COALESCE(sender_session, ''), alias, hop FROM messages WHERE id = ?", id).Scan(&session, &alias, &hop); err != nil {
		return recipient{}, fmt.Errorf("read bounced message: %w", err)
	}
	if session == "" {
		return recipient{}, nil
	}
	holder, err := heldAlias(ctx, c, session)
	if err != nil {
		return recipient{}, fmt.Errorf("read sender alias: %w", err)
	}
	if holder == "" {
		return recipient{}, nil
	}
	body := fmt.Sprintf("your message #%d to %s %s", id, alias, what)
	if _, err := c.ExecContext(ctx, `
		INSERT INTO messages(alias, sender_session, sender_label, body, hop, origin, cause_id, created_at)
		VALUES (?, NULL, 'pager', ?, ?, ?, ?, ?)`,
		holder, body, hop, deliver.OriginCaused, id, now); err != nil {
		return recipient{}, fmt.Errorf("write bounce: %w", err)
	}
	return recipient{session: session, alias: holder, label: "pager"}, nil
}

// ForceRemovePeer forgets a peer even though mail to it has no final result.
//
// Every such row is closed as cancelled and its sender told — with the caveat
// that a transport may already have carried it, so it may still arrive. The
// rows are kept, to age out like any other, and are never sent again: a peer
// added later under the same name starts a new link with nothing pending.
func ForceRemovePeer(ctx context.Context, st *store.Store, peer string, diag io.Writer) error {
	name, err := deliver.NormalizePeerName(peer)
	if err != nil {
		return err
	}
	var bounced []recipient
	var cancelled int
	err = st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		bounced, cancelled = bounced[:0], 0
		if _, err := readPeer(ctx, c, name); err != nil {
			// An unknown peer is a person's mistake here, not a transport's,
			// so it exits 1 as plain peer rm does.
			var ee *ExitError
			if errors.As(err, &ee) {
				return ee.Err
			}
			return err
		}
		rows, err := c.QueryContext(ctx,
			"SELECT id FROM messages WHERE dst_peer = ? AND relayed_at IS NULL ORDER BY dst_seq", name)
		if err != nil {
			return fmt.Errorf("read pending mail: %w", err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		now := st.Now()
		for _, id := range ids {
			if _, err := c.ExecContext(ctx,
				"UPDATE messages SET relayed_at = ?, relay_result = ? WHERE id = ? AND relayed_at IS NULL",
				now, "cancelled: peer removed (may still arrive)", id); err != nil {
				return fmt.Errorf("cancel message %d: %w", id, err)
			}
			cancelled++
			to, err := bounce(ctx, c, now, id,
				"was cancelled: peer "+name+" was removed before it confirmed delivery, so it may still arrive")
			if err != nil {
				return err
			}
			if to.alias != "" {
				bounced = append(bounced, to)
			}
		}
		if _, err := c.ExecContext(ctx, "DELETE FROM peers WHERE name = ?", name); err != nil {
			return fmt.Errorf("remove peer %q: %w", name, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(diag, "removed peer %s; cancelled %d, bounced %d\n", name, cancelled, len(bounced))
	pokeAll(context.WithoutCancel(ctx), bounced, diag)
	return nil
}

// ForceRotateStoreID gives this store a new id after cancelling every message
// to every peer that has no final result, telling each sender it may still
// arrive. See deliver.RotateStoreID for why pending mail cannot simply move to
// the new id.
func ForceRotateStoreID(ctx context.Context, st *store.Store, diag io.Writer) (string, error) {
	var id string
	var bounced []recipient
	var cancelled int
	err := st.WriteTx(ctx, func(ctx context.Context, c *sql.Conn) error {
		bounced, cancelled = bounced[:0], 0
		rows, err := c.QueryContext(ctx,
			"SELECT id, dst_peer FROM messages WHERE dst_peer IS NOT NULL AND relayed_at IS NULL ORDER BY id")
		if err != nil {
			return fmt.Errorf("read pending mail: %w", err)
		}
		type row struct {
			id   int64
			peer string
		}
		var pending []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.peer); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		now := st.Now()
		for _, r := range pending {
			if _, err := c.ExecContext(ctx,
				"UPDATE messages SET relayed_at = ?, relay_result = ? WHERE id = ? AND relayed_at IS NULL",
				now, "cancelled: store id replaced (may still arrive)", r.id); err != nil {
				return fmt.Errorf("cancel message %d: %w", r.id, err)
			}
			cancelled++
			to, err := bounce(ctx, c, now, r.id,
				"was cancelled: this store took a new id before peer "+r.peer+" confirmed delivery, so it may still arrive")
			if err != nil {
				return err
			}
			if to.alias != "" {
				bounced = append(bounced, to)
			}
		}
		id, err = deliver.RelinkAll(ctx, c)
		return err
	})
	if err != nil {
		return "", err
	}
	fmt.Fprintf(diag, "new store id %s; cancelled %d, bounced %d\n", id, cancelled, len(bounced))
	pokeAll(context.WithoutCancel(ctx), bounced, diag)
	return id, nil
}
