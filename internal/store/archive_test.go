package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// insertHuman adds a --human message: no sending session, no cause. delivered
// controls whether it also carries delivery bookkeeping.
func insertHuman(t *testing.T, s *Store, body string, delivered bool) int64 {
	t.Helper()
	now := s.Now()
	var (
		deliveredAt any
		seq         any
	)
	if delivered {
		deliveredAt, seq = now, int64(3)
	}
	res, err := s.db.ExecContext(t.Context(), `
		INSERT INTO messages(alias, body, hop, origin, created_at, delivered_at, delivery_seq)
		VALUES ('gupa', ?, 0, 'human', ?, ?, ?)`, body, now, deliveredAt, seq)
	if err != nil {
		t.Fatalf("insert human message: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// insertCaused adds a reply: it has a sending session and a cause, so the
// schema CHECK forces origin 'caused'.
func insertCaused(t *testing.T, s *Store, cause int64) int64 {
	t.Helper()
	now := s.Now()
	res, err := s.db.ExecContext(t.Context(), `
		INSERT INTO messages(alias, sender_session, sender_label, body, hop, origin,
		                     cause_id, created_at, delivered_at, delivery_seq)
		VALUES ('gupa', 'sess-1', 'foo', 'reply', 1, 'caused', ?, ?, ?, 9)`,
		cause, now, now)
	if err != nil {
		t.Fatalf("insert caused message: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// exportLines dumps the store and returns one raw JSON line per message.
func exportLines(t *testing.T, s *Store) []string {
	t.Helper()
	var buf bytes.Buffer
	n, err := s.ExportAll(t.Context(), &buf)
	if err != nil {
		t.Fatalf("ExportAll: %v", err)
	}
	out := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if buf.Len() == 0 {
		out = nil
	}
	if len(out) != n {
		t.Fatalf("ExportAll reported %d records but wrote %d lines", n, len(out))
	}
	return out
}

// TestArchiveEnabledParsing pins the one switch that can throw messages away.
//
// The asymmetry is deliberate and worth locking down: "off" in any casing or
// padding disables archiving, and everything else — including values a person
// might reasonably expect to work, like "0" and "false" — leaves it on. A typo
// must never be the reason an archive stopped being written.
func TestArchiveEnabledParsing(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"off", false},
		{"OFF", false},
		{"  off  ", false},
		{"Off", false},
		{"", true},
		{"0", true},
		{"false", true},
		{"no", true},
		{"on", true},
		{"offf", true},
		{"nonsense", true},
	} {
		t.Setenv("PAGER_ARCHIVE", tc.value)
		if got := archiveEnabled(); got != tc.want {
			t.Errorf("PAGER_ARCHIVE=%q: archiveEnabled() = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// TestRecordEncodingIsGolden fixes the exact bytes of one record.
//
// Whole-line deduplication is only meaningful if the same row always encodes
// identically, so the encoding is a contract rather than an implementation
// detail. The body deliberately contains <, > and & (which the default encoder
// would escape), a quote, a newline and non-ASCII text.
func TestRecordEncodingIsGolden(t *testing.T) {
	r := Record{
		V:           archiveVersion,
		ID:          7,
		CreatedAt:   "2026-07-19T04:12:33.481Z",
		Alias:       "gupa",
		SenderLabel: "human",
		Origin:      "human",
		Hop:         0,
		Body:        "a <b> & \"c\"\n두 번째 줄",
	}
	want := `{"v":1,"id":7,"created_at":"2026-07-19T04:12:33.481Z","alias":"gupa",` +
		`"sender_session":null,"sender_label":"human","origin":"human","cause_id":null,` +
		`"hop":0,"delivered_at":null,"delivery_seq":null,"listed_at":null,` +
		`"body":"a <b> & \"c\"\n두 번째 줄"}` + "\n"

	var first, second bytes.Buffer
	if err := encodeRecord(&first, r); err != nil {
		t.Fatalf("encodeRecord: %v", err)
	}
	if err := encodeRecord(&second, r); err != nil {
		t.Fatalf("encodeRecord: %v", err)
	}
	if first.String() != want {
		t.Errorf("encoded record\n got: %s\nwant: %s", first.String(), want)
	}
	if first.String() != second.String() {
		t.Errorf("encoding is not deterministic:\n first: %s\nsecond: %s", first.String(), second.String())
	}
}

// TestRecordContract checks the parts of the format a consumer is entitled to
// rely on, one at a time.
//
// A test that only counted records would pass while the shape drifted, and the
// archive is the one artefact here that outlives the database.
func TestRecordContract(t *testing.T) {
	s, _ := newStore(t)

	body := "line one\n인용 \"부호\" & <각괄호>"
	humanID := insertHuman(t, s, body, true)
	causedID := insertCaused(t, s, humanID)
	pendingID := insertHuman(t, s, "not delivered yet", false)

	lines := exportLines(t, s)
	if len(lines) != 3 {
		t.Fatalf("got %d records, want 3", len(lines))
	}

	byID := map[int64]map[string]any{}
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("record is not valid JSON: %v\n%s", err, line)
		}
		id, ok := m["id"].(float64)
		if !ok {
			t.Fatalf("record has no numeric id: %s", line)
		}
		byID[int64(id)] = m
	}

	wantKeys := []string{
		"v", "id", "created_at", "alias", "sender_session", "sender_label",
		"origin", "cause_id", "hop", "delivered_at", "delivery_seq", "listed_at", "body",
	}
	for id, m := range byID {
		// ① the key set is exact — neither a missing field nor a stray one.
		if len(m) != len(wantKeys) {
			t.Errorf("record %d has %d keys, want %d: %v", id, len(m), len(wantKeys), keysOf(m))
		}
		for _, k := range wantKeys {
			if _, ok := m[k]; !ok {
				t.Errorf("record %d is missing %q", id, k)
			}
		}
		// ② the format version travels with every line.
		if m["v"] != float64(archiveVersion) {
			t.Errorf("record %d has v = %v, want %d", id, m["v"], archiveVersion)
		}
		// ③ delivery-lease bookkeeping never reaches the file.
		for _, k := range []string{"claim_token", "claim_epoch", "claimed_at", "claim_expires_at"} {
			if _, ok := m[k]; ok {
				t.Errorf("record %d leaks %q", id, k)
			}
		}
		// ⑤ sender_label is NOT NULL in the schema and must stay a string.
		if _, ok := m["sender_label"].(string); !ok {
			t.Errorf("record %d has non-string sender_label %v", id, m["sender_label"])
		}
	}

	// ④ every nullable column is written as an explicit null, not omitted.
	human := byID[humanID]
	if human["sender_session"] != nil {
		t.Errorf("--human record should have null sender_session, got %v", human["sender_session"])
	}
	if human["cause_id"] != nil {
		t.Errorf("--human record should have null cause_id, got %v", human["cause_id"])
	}
	// ⑥ an undelivered row has both delivery fields null.
	pending := byID[pendingID]
	if pending["delivered_at"] != nil || pending["delivery_seq"] != nil {
		t.Errorf("undelivered record should have null delivered_at and delivery_seq, got %v and %v",
			pending["delivered_at"], pending["delivery_seq"])
	}

	// A caused record carries the non-null counterparts, so the nulls above are
	// really about the value and not about the field always being empty.
	caused := byID[causedID]
	if caused["sender_session"] != "sess-1" || caused["cause_id"] != float64(humanID) {
		t.Errorf("caused record lost its attribution: sender_session=%v cause_id=%v",
			caused["sender_session"], caused["cause_id"])
	}
	if caused["hop"] != float64(1) {
		t.Errorf("caused record has hop = %v, want 1", caused["hop"])
	}

	// ⑦ the timestamp is lossless against the integer the schema stores, and
	// its width is fixed.
	var createdMS int64
	if err := s.db.QueryRowContext(t.Context(),
		"SELECT created_at FROM messages WHERE id = ?", humanID).Scan(&createdMS); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	stamp, _ := human["created_at"].(string)
	parsed, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatalf("created_at %q does not parse: %v", stamp, err)
	}
	if parsed.UnixMilli() != createdMS {
		t.Errorf("created_at %q is %d ms, want %d", stamp, parsed.UnixMilli(), createdMS)
	}
	if !strings.HasSuffix(stamp, "Z") || len(stamp) != len("2006-01-02T15:04:05.000Z") {
		t.Errorf("created_at %q is not fixed-width UTC with three fractional digits", stamp)
	}

	// ⑧ the body survives newlines, quotes, ampersands and non-ASCII intact.
	if human["body"] != body {
		t.Errorf("body did not round-trip:\n got: %q\nwant: %q", human["body"], body)
	}
}

// sampleRecords builds n distinct records without needing a database.
func sampleRecords(n int) []Record {
	out := make([]Record, n)
	for i := range out {
		out[i] = Record{
			V:           archiveVersion,
			ID:          int64(i + 1),
			CreatedAt:   archiveStamp(testBase.UnixMilli() + int64(i)),
			Alias:       "gupa",
			SenderLabel: "human",
			Origin:      "human",
			Body:        fmt.Sprintf("message %d", i+1),
		}
	}
	return out
}

// TestArchiveCorrectsExistingFileMode covers the case O_CREATE cannot: a file
// that is already there.
//
// The archive accumulates message bodies written by other sessions, so a
// world-readable one is the same leak the database guards against — and
// O_CREATE's mode argument is ignored entirely when the file exists.
func TestArchiveCorrectsExistingFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), archiveName)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("pre-create archive: %v", err)
	}

	if err := appendArchive(path, sampleRecords(1)); err != nil {
		t.Fatalf("appendArchive: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat archive: %v", err)
	}
	if got := info.Mode().Perm(); got != filePerm {
		t.Errorf("archive mode is %04o, want %04o", got, filePerm)
	}
}

// TestArchiveCreatesFileWith0600 covers the create branch, which the
// existing-file test cannot reach.
//
// O_CREATE's mode is filtered by umask, so the explicit Chmod has to run on a
// brand-new file too. Without this, making that Chmod conditional on the file
// already existing would pass every other test while leaving a fresh archive
// world-readable.
func TestArchiveCreatesFileWith0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), archiveName)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("archive already exists before the test (stat error: %v)", err)
	}

	if err := appendArchive(path, sampleRecords(1)); err != nil {
		t.Fatalf("appendArchive: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat archive: %v", err)
	}
	if got := info.Mode().Perm(); got != filePerm {
		t.Errorf("freshly created archive mode is %04o, want %04o", got, filePerm)
	}
}

// TestAppendRepairsTruncatedTail proves a short write cannot corrupt the file
// permanently.
//
// Without the repair, the next append lands directly after the fragment and the
// two halves read as one malformed line — which line-wise deduplication would
// happily treat as a record. The fragment's own message is not lost by dropping
// it: nothing is deleted until an append succeeds, so the next pass writes it
// again in full.
func TestAppendRepairsTruncatedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), archiveName)

	if err := appendArchive(path, sampleRecords(2)); err != nil {
		t.Fatalf("appendArchive: %v", err)
	}
	// Simulate a write that stopped partway through the third record.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	if _, err := f.WriteString(`{"v":1,"id":3,"created_at":"2026-08`); err != nil {
		t.Fatalf("write fragment: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}

	if err := appendArchive(path, sampleRecords(3)[2:]); err != nil {
		t.Fatalf("appendArchive after fragment: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Error("archive does not end with a newline")
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), data)
	}
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("line %d is not valid JSON: %v\n%s", i+1, err, line)
			continue
		}
		if m["id"] != float64(i+1) {
			t.Errorf("line %d has id %v, want %d", i+1, m["id"], i+1)
		}
	}
}

// TestAppendRepairsFileWithNoNewlineAtAll is the degenerate case of the same
// rule: when the fragment is the whole file, there is nothing to keep.
func TestAppendRepairsFileWithNoNewlineAtAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), archiveName)
	if err := os.WriteFile(path, []byte(`{"v":1,"id":1`), filePerm); err != nil {
		t.Fatalf("pre-create archive: %v", err)
	}

	if err := appendArchive(path, sampleRecords(1)); err != nil {
		t.Fatalf("appendArchive: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1:\n%s", len(lines), data)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Errorf("surviving line is not valid JSON: %v\n%s", err, lines[0])
	}
}

// archivedIDs reads the ids currently recorded in a store's archive file.
func archivedIDs(t *testing.T, s *Store) []int64 {
	t.Helper()
	data, err := os.ReadFile(archivePath(s.path))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	var out []int64
	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("archive line is not valid JSON: %v\n%s", err, line)
		}
		out = append(out, m.ID)
	}
	return out
}

// TestArchivesBeforeDeleting is the whole point of the feature: what prune
// removes has to exist somewhere else first.
func TestArchivesBeforeDeleting(t *testing.T) {
	// Pin the switch rather than inheriting it. A developer with PAGER_ARCHIVE
	// set in their shell would otherwise see this pass or fail for a reason
	// that has nothing to do with the code.
	t.Setenv("PAGER_ARCHIVE", "")
	s, _ := newStore(t)
	want := []int64{
		insertDelivered(t, s, DefaultRetention+time.Hour),
		insertDelivered(t, s, DefaultRetention+2*time.Hour),
	}

	res, err := s.PruneNow(t.Context(), DefaultRetention, false)
	if err != nil {
		t.Fatalf("PruneNow: %v", err)
	}
	if res.Deleted != int64(len(want)) {
		t.Fatalf("deleted %d, want %d", res.Deleted, len(want))
	}

	got := archivedIDs(t, s)
	if len(got) != len(want) {
		t.Fatalf("archive holds %v, want %v", got, want)
	}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("archive[%d] = %d, want %d", i, got[i], id)
		}
		if messageExists(t, s, id) {
			t.Errorf("message %d survived the prune", id)
		}
	}
}

// TestDeleteSkipsRowsReferencedSinceRead covers the window the predicate
// re-application exists for.
//
// Between reading the candidates and deleting them, a reply can arrive and
// point cause_id at one of them. foreign_keys is ON, so a DELETE that named the
// ids alone would be rejected in full and prune would stop making progress
// entirely. Re-applying the predicate drops just that row.
func TestDeleteSkipsRowsReferencedSinceRead(t *testing.T) {
	s, _ := newStore(t)
	referenced := insertDelivered(t, s, DefaultRetention+time.Hour)
	free := insertDelivered(t, s, DefaultRetention+2*time.Hour)

	cutoff := s.Now() - DefaultRetention.Milliseconds()
	recs, err := s.selectDeletable(t.Context(), cutoff, pruneBatch)
	if err != nil {
		t.Fatalf("selectDeletable: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("selected %d candidates, want 2", len(recs))
	}

	// The reply lands after the read. It is fresh, so it is not itself a
	// candidate — it only makes `referenced` undeletable.
	insertCaused(t, s, referenced)

	n, err := s.deleteArchived(t.Context(), recs, cutoff)
	if err != nil {
		t.Fatalf("deleteArchived: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d rows, want 1", n)
	}
	if !messageExists(t, s, referenced) {
		t.Errorf("message %d was deleted even though a reply now cites it", referenced)
	}
	if messageExists(t, s, free) {
		t.Errorf("message %d should have been deleted", free)
	}
}

// TestKeepsMessagesWhenArchiveWriteFails is the failure the ordering exists
// for: if the archive cannot be written, nothing may be deleted.
//
// Deleting anyway would turn a full disk into permanent data loss, which is
// precisely what this feature is here to prevent. The cost of refusing is that
// prune stops making progress until the write can succeed — a deliberate trade
// of silent loss for visible growth.
func TestKeepsMessagesWhenArchiveWriteFails(t *testing.T) {
	t.Setenv("PAGER_ARCHIVE", "")
	s, _ := newStore(t)
	id := insertDelivered(t, s, DefaultRetention+time.Hour)
	before := countMessages(t, s)

	// A directory standing where the archive file belongs makes the open fail.
	// Chmod would not: a test running as root would sail straight through it.
	if err := os.Mkdir(archivePath(s.path), 0o700); err != nil {
		t.Fatalf("block archive path: %v", err)
	}

	if _, err := s.PruneNow(t.Context(), DefaultRetention, false); err == nil {
		t.Fatal("PruneNow reported success although the archive could not be written")
	}
	if got := countMessages(t, s); got != before {
		t.Errorf("message count is %d after a failed archive, want %d", got, before)
	}
	if !messageExists(t, s, id) {
		t.Errorf("message %d was deleted without being archived", id)
	}
}

// TestArchiveOffSkipsTheFile checks the escape hatch actually escapes: no file
// is created and the delete behaves as it did before archiving existed.
func TestArchiveOffSkipsTheFile(t *testing.T) {
	t.Setenv("PAGER_ARCHIVE", "off")
	s, _ := newStore(t)
	id := insertDelivered(t, s, DefaultRetention+time.Hour)

	res, err := s.PruneNow(t.Context(), DefaultRetention, false)
	if err != nil {
		t.Fatalf("PruneNow: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("deleted %d, want 1", res.Deleted)
	}
	if messageExists(t, s, id) {
		t.Errorf("message %d survived the prune", id)
	}
	if _, err := os.Stat(archivePath(s.path)); !os.IsNotExist(err) {
		t.Errorf("archive file exists with PAGER_ARCHIVE=off (stat error: %v)", err)
	}
}

// TestExportWritesEveryMessage checks the export is complete and ordered.
//
// The order is contractual: an export that came out in a different order every
// run could not be diffed against the previous one, which is most of what a
// person wants from it.
func TestExportWritesEveryMessage(t *testing.T) {
	s, _ := newStore(t)
	var want []int64
	for i := range 5 {
		want = append(want, insertHuman(t, s, fmt.Sprintf("body %d", i), i%2 == 0))
	}

	lines := exportLines(t, s)
	if len(lines) != countMessages(t, s) {
		t.Fatalf("exported %d lines for %d messages", len(lines), countMessages(t, s))
	}

	var got []int64
	for _, line := range lines {
		var m struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("export line is not valid JSON: %v\n%s", err, line)
		}
		got = append(got, m.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("exported ids %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("export[%d] = %d, want %d (ids must ascend)", i, got[i], want[i])
		}
	}
}

// TestExportMatchesArchiveShape is what makes the two producers composable.
//
// Concatenating an archive with a live export and removing repeated lines is
// the documented way to read the full history. That only works if a given
// message encodes to exactly the same bytes whichever path emitted it.
func TestExportMatchesArchiveShape(t *testing.T) {
	t.Setenv("PAGER_ARCHIVE", "")
	s, _ := newStore(t)
	insertDelivered(t, s, DefaultRetention+time.Hour)

	exported := exportLines(t, s)
	if len(exported) != 1 {
		t.Fatalf("exported %d lines, want 1", len(exported))
	}

	if _, err := s.PruneNow(t.Context(), DefaultRetention, false); err != nil {
		t.Fatalf("PruneNow: %v", err)
	}
	data, err := os.ReadFile(archivePath(s.path))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	archived := strings.TrimSuffix(string(data), "\n")

	if archived != exported[0] {
		t.Errorf("the same message encodes differently:\n archive: %s\n  export: %s", archived, exported[0])
	}
}

// TestArchiveRecordsTheListedState covers the state an export would otherwise
// lose: listed_at is not recoverable from any other field, so without it a
// backup cannot tell a message nobody touched from one an agent read and left.
//
// The three cases are the whole invariant, and the middle one is the reason the
// obvious shortcut is wrong. "A delivered row has listed_at NULL" is NOT true:
// the hook deliberately re-injects a polled message, so polled-then-delivered is
// an ordinary outcome and the row carries both columns. Asserting the shortcut
// would have written a test that later traps correct behaviour.
func TestArchiveRecordsTheListedState(t *testing.T) {
	s, _ := newStore(t)

	untouched := insertHuman(t, s, "nobody has looked at this", false)
	polled := insertHuman(t, s, "an agent polled this", false)
	polledThenDelivered := insertHuman(t, s, "polled, then a hook injected it", true)

	stamp := s.Now()
	if _, err := s.Exec(t.Context(),
		"UPDATE messages SET listed_at = ? WHERE id IN (?, ?)",
		stamp, polled, polledThenDelivered); err != nil {
		t.Fatalf("stamp listed_at: %v", err)
	}

	byID := map[int64]map[string]any{}
	for _, line := range exportLines(t, s) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("record is not valid JSON: %v\n%s", err, line)
		}
		byID[int64(m["id"].(float64))] = m
	}

	for _, tc := range []struct {
		name string
		id   int64
		want bool // listed_at is present
	}{
		{"never listed", untouched, false},
		{"polled while waiting", polled, true},
		{"polled, then delivered — carries both", polledThenDelivered, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := byID[tc.id]
			if !ok {
				t.Fatalf("message #%d is missing from the export", tc.id)
			}
			if _, present := m["listed_at"]; !present {
				t.Fatal("the record has no listed_at key at all")
			}
			got := m["listed_at"] != nil
			if got != tc.want {
				t.Errorf("listed_at present = %v, want %v (value %v)", got, tc.want, m["listed_at"])
			}
		})
	}

	// The both-columns row is the one worth stating outright: an export that kept
	// only delivered_at would flatten it into the never-polled case.
	if both := byID[polledThenDelivered]; both["delivered_at"] == nil || both["listed_at"] == nil {
		t.Errorf("polled-then-delivered record lost an axis: delivered_at=%v listed_at=%v",
			both["delivered_at"], both["listed_at"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
