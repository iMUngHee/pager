package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// archiveVersion travels on every line so a reader never has to infer the
// format. Fields may be added without bumping it; removing, renaming or
// redefining one does bump it.
const archiveVersion = 1

// archiveName is the file that lives beside the database. It is not derived
// from the environment: a store archives to whatever database it actually
// opened, so isolating a test or a second set of sessions with PAGER_DB
// isolates its archive too.
const archiveName = "archive.jsonl"

// archiveTime renders a timestamp as RFC3339 in UTC with exactly three
// fractional digits.
//
// Milliseconds are what the schema stores, so this is lossless. Fixing the
// digit count is what makes string ordering agree with time ordering — a
// variable-width fraction sorts "…:05.9Z" after "…:05.10Z".
const archiveTime = "2006-01-02T15:04:05.000Z07:00"

// Record is one message as the archive and `pager export` write it.
//
// The four claim_* columns are deliberately absent. They are delivery-lease
// bookkeeping, spent once a message has been delivered, and a claim token is not
// something to leave in a file. Note that "already delivered" describes what
// prune archives, not everything this struct carries: ExportAll selects every
// row, undelivered ones included, so the two delivery fields below are genuinely
// nullable here.
//
// Field order here is the field order on the wire: encoding/json emits struct
// fields in declaration order. That, plus the encoder in encodeRecord, is what
// makes two encodings of the same row byte-identical, which is what lets a
// reader deduplicate whole lines. Adding a field changes those bytes, so a
// record written before the addition and the same row written after it no longer
// fold together — archiveVersion tolerates that (see its comment) and README
// documents the dedup as a convenience rather than a guarantee.
type Record struct {
	V             int     `json:"v"`
	ID            int64   `json:"id"`
	CreatedAt     string  `json:"created_at"`
	Alias         string  `json:"alias"`
	SenderSession *string `json:"sender_session"`
	SenderLabel   string  `json:"sender_label"`
	Origin        string  `json:"origin"`
	CauseID       *int64  `json:"cause_id"`
	Hop           int     `json:"hop"`
	DeliveredAt   *string `json:"delivered_at"`
	DeliverySeq   *int64  `json:"delivery_seq"`
	// ListedAt is when an agent first pulled this message up in a listing of its
	// own inbox, while it was still undelivered. It is the other half of having
	// been read, and it is exported because it is not recoverable from anything
	// else in the record: without it a backup cannot tell a message nobody
	// touched from one an agent read and left.
	ListedAt *string `json:"listed_at"`
	Body     string  `json:"body"`
}

// recordColumns is the SELECT list Record scans, in Record's own order.
const recordColumns = `id, created_at, alias, sender_session, sender_label,
	origin, cause_id, hop, delivered_at, delivery_seq, listed_at, body`

// archivePath is where a database's archive lives.
func archivePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), archiveName)
}

// archiveEnabled reports whether prune should archive before deleting.
//
// Only the literal word "off" disables it, after trimming and case folding —
// the same normalisation PAGER_CLIENT gets. Every other value, including "0"
// and "false", leaves archiving on. Recognising more spellings would only widen
// the set of typos that silently throw messages away, and the safe side of this
// switch is on.
func archiveEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("PAGER_ARCHIVE")), "off")
}

// encodeRecord writes one record as a single line, newline included.
//
// SetEscapeHTML(false) is not cosmetic. The default escapes <, > and & to \u
// sequences, so a body containing them would encode differently depending on
// which encoder produced it — and whole-line deduplication would stop working
// exactly for the messages most likely to be repeated.
func encodeRecord(w io.Writer, r Record) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// archiveStamp renders a schema timestamp for a record.
func archiveStamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(archiveTime)
}

// repairChunk bounds how much is read at a time when scanning back for the last
// newline. Message bodies have no length limit in the schema, so a fragment can
// in principle be long.
const repairChunk = 64 << 10

// appendArchive adds records to the archive file and returns once they are on
// disk.
//
// The whole batch is encoded before the file is touched, so a record that
// cannot be encoded fails the call rather than leaving half a batch behind.
func appendArchive(path string, recs []Record) error {
	var buf bytes.Buffer
	for _, r := range recs {
		if err := encodeRecord(&buf, r); err != nil {
			return fmt.Errorf("encode record %d: %w", r.ID, err)
		}
	}

	// These wraps name the operation but not the path: every error below is an
	// *os.PathError, which already carries it. Adding it again is how a failure
	// people are told to read — `pager prune` is the one place an archive
	// failure is visible — ends up saying "open X: open X: is a directory".
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close() //nolint:errcheck // Sync below is the durability point

	// O_CREATE's mode is filtered by umask and does nothing at all to a file
	// that already exists, so state the permission explicitly in both cases —
	// the same reason Open does it for the database. This file accumulates
	// message bodies written by other sessions.
	if err := f.Chmod(filePerm); err != nil {
		return fmt.Errorf("chmod archive: %w", err)
	}

	end, err := repairTail(f)
	if err != nil {
		return fmt.Errorf("repair archive: %w", err)
	}
	if _, err := f.WriteAt(buf.Bytes(), end); err != nil {
		return fmt.Errorf("write archive: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync archive: %w", err)
	}
	return nil
}

// repairTail removes a partially written final record and reports where the
// next write belongs.
//
// A short write leaves the file without a trailing newline; a full disk is the
// realistic cause, and that is one of the failure modes archiving exists to
// survive. Appending after such a fragment would glue the next record onto it
// and produce a single unparsable line that nothing downstream could recognise
// — line-wise deduplication would treat it as an ordinary record.
//
// Dropping the fragment costs nothing: its message was never deleted, because
// the delete only ever runs after this write succeeded. The next pass archives
// it again, whole.
func repairTail(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	if size == 0 {
		return 0, nil
	}

	var last [1]byte
	if _, err := f.ReadAt(last[:], size-1); err != nil {
		return 0, err
	}
	if last[0] == '\n' {
		return size, nil
	}

	// No trailing newline: scan back for the last one. If there is none at all,
	// the entire file is a fragment and cut stays 0.
	var cut int64
	for off := size; off > 0; {
		n := min(int64(repairChunk), off)
		off -= n
		b := make([]byte, n)
		if _, err := f.ReadAt(b, off); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			cut = off + int64(i) + 1
			break
		}
	}
	if err := f.Truncate(cut); err != nil {
		return 0, err
	}
	return cut, nil
}

// eachRecord scans rows selected with recordColumns and hands each one to fn.
//
// Streaming rather than returning a slice is what lets export dump a whole
// database without holding it in memory; prune collects into a slice because it
// needs the ids again afterwards.
func eachRecord(rows *sql.Rows, fn func(Record) error) error {
	for rows.Next() {
		var (
			r         Record
			created   int64
			sender    sql.NullString
			cause     sql.NullInt64
			delivered sql.NullInt64
			seq       sql.NullInt64
			listed    sql.NullInt64
		)
		if err := rows.Scan(&r.ID, &created, &r.Alias, &sender, &r.SenderLabel,
			&r.Origin, &cause, &r.Hop, &delivered, &seq, &listed, &r.Body); err != nil {
			return fmt.Errorf("scan message: %w", err)
		}
		r.V = archiveVersion
		r.CreatedAt = archiveStamp(created)
		if sender.Valid {
			r.SenderSession = &sender.String
		}
		if cause.Valid {
			r.CauseID = &cause.Int64
		}
		if delivered.Valid {
			stamp := archiveStamp(delivered.Int64)
			r.DeliveredAt = &stamp
		}
		if seq.Valid {
			r.DeliverySeq = &seq.Int64
		}
		if listed.Valid {
			stamp := archiveStamp(listed.Int64)
			r.ListedAt = &stamp
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ExportAll writes every stored message to w as JSONL, ordered by id, and
// reports how many it wrote.
//
// The order is part of the contract rather than an accident: an export nobody
// can diff against the previous one is a backup, not a record.
func (s *Store) ExportAll(ctx context.Context, w io.Writer) (int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+recordColumns+" FROM messages ORDER BY id")
	if err != nil {
		return 0, fmt.Errorf("export messages: %w", err)
	}
	defer rows.Close()

	n := 0
	err = eachRecord(rows, func(r Record) error {
		if err := encodeRecord(w, r); err != nil {
			return fmt.Errorf("write record %d: %w", r.ID, err)
		}
		n++
		return nil
	})
	return n, err
}
