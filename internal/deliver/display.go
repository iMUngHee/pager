package deliver

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLabelRunes caps how much of a sender label is ever shown.
const MaxLabelRunes = 64

// SanitizeLabel is a sender label as it may be displayed: one line, every
// control or line-separating character escaped, at most MaxLabelRunes runes.
//
// It runs at display time on whatever is stored, so labels written before this
// existed are covered too. The point is structural rather than cosmetic: the
// lines around a sender header — the quoted body, the provenance line under a
// relayed message — are written by pager itself, and a label that could break
// the line could forge them.
func SanitizeLabel(label string) string {
	return clean(label, MaxLabelRunes)
}

// clean folds s to one line of at most limit runes, escaping what could end
// or reshape a line. Invalid UTF-8 is replaced rather than passed through.
func clean(s string, limit int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == limit {
			b.WriteString("…")
			break
		}
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || unicode.In(r, unicode.Cf):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
		n++
	}
	return b.String()
}

// ProvenanceLine is the line pager writes under a message that arrived from
// another store, or "" for local mail. It is decided by the src_* columns, never
// by the label: a label is one line by construction, so no label can produce or
// imitate a line of its own.
func ProvenanceLine(srcPeer, srcFrom string) string {
	if srcPeer == "" {
		return ""
	}
	// Both were validated on the way in; cleaning them again here keeps this
	// line one line even if a row reached the store some other way.
	srcPeer, srcFrom = clean(srcPeer, maxPeerName), clean(srcFrom, 128)
	if srcFrom == "" {
		return "relayed from peer " + srcPeer + " — no reply address; answer in your own output"
	}
	return "relayed from peer " + srcPeer + " — reply to " + srcFrom + "@" + srcPeer
}

// lineBreaks maps everything a reader might take for the end of a line onto
// "\n": CRLF and a bare CR (which returns a terminal's cursor to the start of
// the row), the Unicode line and paragraph separators, NEL, vertical tab and
// form feed.
var lineBreaks = strings.NewReplacer(
	"\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u2029", "\n", "\u0085", "\n", "\v", "\n", "\f", "\n")

// BodyLines splits a body into the lines a reader could see. Splitting on
// "\n" alone would let a body that uses any other line break start a line
// that is not marked as body — one that could pass for pager's own.
func BodyLines(body string) []string {
	return strings.Split(lineBreaks.Replace(body), "\n")
}

// CleanReason folds a relay reason — text another store wrote — to one line
// fit to quote in a bounce.
func CleanReason(reason string) string {
	return clean(reason, 300)
}
