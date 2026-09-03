package deliver

import "strings"

// PurposeLimit is how much of a prompt the roster keeps.
//
// It is a context budget rather than a display width. msg_roster renders one of
// these per live session into an agent's context, and a prompt has no length of
// its own — someone pasting a stack trace would otherwise put the whole thing in
// front of every session on the machine.
const PurposeLimit = 80

// Purpose folds a prompt into the single short line a roster row can carry.
//
// Whitespace collapses because the roster is a tabwriter table and one surviving
// tab shifts every column of its row, and because a prompt spanning three lines
// says no more about the work than its first sentence does.
//
// The cut counts runes, not bytes. Prompts here are routinely Korean, and a byte
// cut lands mid-character: the column every agent reads would fill with
// replacement characters at exactly the length that matters most.
//
// This is the only producer of the column, so it is also the only place the
// limit is enforced. The renderer deliberately does not re-cut — a second
// enforcement point would be a second definition of the limit, and a row longer
// than this in the database means the write path is broken and should be visible
// rather than hidden by the display.
func Purpose(prompt string) string {
	folded := strings.Join(strings.Fields(prompt), " ")
	runes := []rune(folded)
	if len(runes) <= PurposeLimit {
		return folded
	}
	return string(runes[:PurposeLimit])
}
