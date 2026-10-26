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
	if srcFrom == "" {
		return "relayed from peer " + srcPeer + " — no reply address; answer in your own output"
	}
	return "relayed from peer " + srcPeer + " — reply to " + srcFrom + "@" + srcPeer
}

// CleanReason folds a relay reason — text another store wrote — to one line
// fit to quote in a bounce.
func CleanReason(reason string) string {
	return clean(reason, 300)
}
