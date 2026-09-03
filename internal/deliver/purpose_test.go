package deliver

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestPurposeFoldsToOneLine covers both halves of what the roster needs from a
// prompt: one line, and a length it can afford.
//
// The rune case is not a nicety. Prompts in this workspace are routinely
// Korean, where a byte cut lands mid-character and the column every agent reads
// fills with replacement characters. utf8.ValidString is the assertion rather
// than a length comparison, because it fails for the reason that matters.
func TestPurposeFoldsToOneLine(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty stays empty", "", ""},
		{"whitespace only is empty", "  \n\t ", ""},
		{"a newline becomes a space", "fix the\nroster", "fix the roster"},
		{"a tab becomes a space", "fix\tthe\troster", "fix the roster"},
		{"runs of whitespace collapse", "fix   the \n\n  roster", "fix the roster"},
		{"the ends are trimmed", "  fix the roster \n", "fix the roster"},
		{"a short line is untouched", "다음 작업 뭐야", "다음 작업 뭐야"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Purpose(tc.in); got != tc.want {
				t.Errorf("Purpose(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	t.Run("a long prompt is cut to the limit", func(t *testing.T) {
		got := Purpose(strings.Repeat("a", PurposeLimit+50))
		if n := utf8.RuneCountInString(got); n != PurposeLimit {
			t.Errorf("cut to %d runes, want %d", n, PurposeLimit)
		}
	})

	// The tab matters twice over: the roster is a tabwriter table, so one
	// surviving tab would shift every column of that row.
	t.Run("no tab survives the fold", func(t *testing.T) {
		if got := Purpose("a\tb"); strings.ContainsAny(got, "\t\n\r") {
			t.Errorf("Purpose kept a control character: %q", got)
		}
	})

	t.Run("a long Korean prompt is cut on a rune boundary", func(t *testing.T) {
		got := Purpose(strings.Repeat("한", PurposeLimit+10))
		if !utf8.ValidString(got) {
			t.Errorf("the cut produced invalid UTF-8: %q", got)
		}
		if n := utf8.RuneCountInString(got); n != PurposeLimit {
			t.Errorf("cut to %d runes, want %d", n, PurposeLimit)
		}
		if strings.ContainsRune(got, '�') {
			t.Errorf("the cut produced a replacement character: %q", got)
		}
	})
}
