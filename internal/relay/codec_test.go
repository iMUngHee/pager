package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestRelayStreamIsASCIIAndRoundTripsKorean checks both halves of the stream
// contract. What pager writes is pure ASCII — so a pipe or console that
// re-encodes text cannot corrupt it — and decodes back to exactly what went
// in. What pager reads may be raw UTF-8 with a byte-order mark and CRLF line
// endings, as another writer or a Windows shell might produce it.
func TestRelayStreamIsASCIIAndRoundTripsKorean(t *testing.T) {
	type payload struct {
		Body  string `json:"body"`
		Label string `json:"label"`
	}
	in := payload{
		Body:  "안녕하세요 — 배포는 내일\n\t🚀 done <ok> & \"quoted\"   end",
		Label: "사페",
	}

	var buf bytes.Buffer
	if err := writeLine(&buf, in); err != nil {
		t.Fatalf("writeLine: %v", err)
	}
	line := buf.Bytes()
	for i, c := range line {
		if c >= 0x80 {
			t.Fatalf("byte %d of the written line is 0x%02x; the stream must be ASCII: %s", i, c, line)
		}
	}
	if bytes.Count(line, []byte("\n")) != 1 || line[len(line)-1] != '\n' {
		t.Fatalf("written record is not exactly one LF-terminated line: %q", line)
	}
	var out payload
	if err := json.Unmarshal(line, &out); err != nil {
		t.Fatalf("decode the written line: %v", err)
	}
	if out != in {
		t.Errorf("round trip changed the record:\n got %+v\nwant %+v", out, in)
	}

	// The reading side: raw UTF-8, a BOM at the start, CRLF endings.
	raw := "\xef\xbb\xbf" + `{"body":"첫 줄"}` + "\r\n" + `{"body":"둘째"}` + "\n"
	lr := newLineReader(strings.NewReader(raw))
	for _, want := range []string{"첫 줄", "둘째"} {
		l, err := lr.next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		var p payload
		if err := json.Unmarshal(l, &p); err != nil {
			t.Fatalf("decode %q: %v", l, err)
		}
		if p.Body != want {
			t.Errorf("body = %q, want %q", p.Body, want)
		}
	}
	if _, err := lr.next(); !errors.Is(err, io.EOF) {
		t.Errorf("after the last line: err = %v, want io.EOF", err)
	}

	// A BOM anywhere but the start is data, not a marker to strip.
	lr = newLineReader(strings.NewReader("{}\n\xef\xbb\xbf{}\n"))
	_, _ = lr.next()
	if l, _ := lr.next(); !bytes.HasPrefix(l, []byte("\xef\xbb\xbf")) {
		t.Errorf("a mid-stream BOM was stripped: %q", l)
	}

	// A tail without its LF is incomplete even when its bytes parse.
	lr = newLineReader(strings.NewReader(`{"body":"a"}` + "\n" + `{"body":"b"}`))
	if _, err := lr.next(); err != nil {
		t.Fatalf("complete first line: %v", err)
	}
	if _, err := lr.next(); !errors.Is(err, errTruncated) {
		t.Errorf("unterminated tail: err = %v, want errTruncated", err)
	}

	// A line past the bound is refused rather than buffered without limit.
	lr = newLineReader(strings.NewReader(strings.Repeat("x", MaxLine+1) + "\n"))
	if _, err := lr.next(); !errors.Is(err, errLineTooLong) {
		t.Errorf("oversized line: err = %v, want errLineTooLong", err)
	}
	lr = newLineReader(strings.NewReader(strings.Repeat("x", MaxLine) + "\r\n"))
	if l, err := lr.next(); err != nil || len(l) != MaxLine {
		t.Errorf("a line exactly at the bound: len %d, err %v", len(l), err)
	}
	// The bound is on content: a leading byte-order mark does not use it up.
	lr = newLineReader(strings.NewReader("\xef\xbb\xbf" + strings.Repeat("x", MaxLine) + "\r\n"))
	if l, err := lr.next(); err != nil || len(l) != MaxLine {
		t.Errorf("a BOM-led line exactly at the bound: len %d, err %v", len(l), err)
	}
}

// endless is a reader that never ends a line, counting what it handed out.
type endless struct{ n int }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	e.n += len(p)
	return len(p), nil
}

// TestLineReaderStopsReadingAtTheBound: a line that never ends is refused
// once it passes the bound, without the reader consuming — and holding — the
// rest of the stream first.
func TestLineReaderStopsReadingAtTheBound(t *testing.T) {
	src := &endless{}
	lr := newLineReader(io.LimitReader(src, 64<<20))
	if _, err := lr.next(); !errors.Is(err, errLineTooLong) {
		t.Fatalf("an endless line: err = %v, want errLineTooLong", err)
	}
	if src.n > MaxLine+2*(64<<10) {
		t.Errorf("read %d bytes before refusing a line bounded at %d", src.n, MaxLine)
	}
}
