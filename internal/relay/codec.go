package relay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
	"unicode/utf8"
)

// The relay stream is JSON Lines that must survive whatever carries it: an ssh
// pipe, a file, a Windows console that re-encodes text between programs. So
// what pager writes is pure ASCII — every non-ASCII character escaped as
// \uXXXX — and what it reads may be anything a reasonable writer produces:
// UTF-8, a byte-order mark at the start, CRLF line endings.

// MaxLine bounds one line of the stream, terminator excluded. MaxRelayBody is
// sized so that a body escaped at six bytes per byte still fits.
const MaxLine = 1 << 20

var (
	// errTruncated is input that ended inside a line. The line is incomplete
	// by definition — even if its bytes happen to parse — so it has no effect.
	errTruncated = errors.New("input ended inside a line")
	// errLineTooLong is a line past MaxLine.
	errLineTooLong = fmt.Errorf("line longer than %d bytes", MaxLine)
)

// writeLine writes v as one line of ASCII JSON.
func writeLine(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // Encode appends the '\n'
		return err
	}
	_, err := w.Write(escapeNonASCII(buf.Bytes()))
	return err
}

// escapeNonASCII rewrites every non-ASCII rune of encoding/json's output as a
// \u escape. That output is valid UTF-8 — invalid input was already replaced
// by U+FFFD — and non-ASCII bytes can only occur inside string literals, where
// a \u escape means the same thing, so the rewrite never changes the value.
func escapeNonASCII(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		if b[0] < utf8.RuneSelf {
			out = append(out, b[0])
			b = b[1:]
			continue
		}
		r, size := utf8.DecodeRune(b)
		if r > 0xFFFF {
			hi, lo := utf16.EncodeRune(r)
			out = fmt.Appendf(out, `\u%04x\u%04x`, hi, lo)
		} else {
			out = fmt.Appendf(out, `\u%04x`, r)
		}
		b = b[size:]
	}
	return out
}

// lineReader yields the complete lines of a stream.
type lineReader struct {
	r       *bufio.Reader
	started bool
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// next returns the next complete line without its LF or CRLF.
//
// It returns io.EOF at a clean end, errTruncated when the stream ends inside a
// line, and errLineTooLong for a line over MaxLine. A byte-order mark is
// accepted only at the very start of the stream, and does not count toward the
// bound. After errLineTooLong the reader is left inside that line and must not
// be read again; every caller stops the run there.
func (lr *lineReader) next() ([]byte, error) {
	limit := MaxLine + 2 // allow for the CRLF itself
	if !lr.started {
		limit += 3 // and a leading byte-order mark
	}
	var line []byte
	for {
		chunk, err := lr.r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > limit {
			return nil, errLineTooLong
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return nil, io.EOF
			}
			return nil, errTruncated
		}
		return nil, err
	}

	if !lr.started {
		lr.started = true
		line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) > MaxLine {
		return nil, errLineTooLong
	}
	return line, nil
}
