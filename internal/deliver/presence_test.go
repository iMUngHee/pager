package deliver

import (
	"strings"
	"testing"
)

// fixedProbe stands in for a real process lookup. The point of injecting the
// probe is that a test can state the answer instead of having to arrange a
// process that produces it — see HostProbe.
func fixedProbe(alive, known bool) HostProbe {
	return func(int, int64) (bool, bool) { return alive, known }
}

// TestPresenceOfSeparatesStrandedFromHeld is the decision this type exists to
// make, and the middle row is the one that matters most.
//
// A probe that could not answer must leave the target Held. Reporting it as
// stranded would tell a sender nobody is behind a name on no evidence at all,
// which hides a live notification — the same rule the HOST column's "unknown"
// follows, and the reason Alive returns two booleans rather than one.
func TestPresenceOfSeparatesStrandedFromHeld(t *testing.T) {
	st, _ := newStore(t)
	inboxSession(t, st, "holder", "held")
	bindHost(t, st, "holder", 4242, 99)
	target := Target{Alias: "held", SessionID: "holder"}

	for _, tc := range []struct {
		name         string
		alive, known bool
		want         Presence
	}{
		{"the process is still there", true, true, PresenceHeld},
		{"the process is gone", false, true, PresenceStranded},
		{"nothing could be asked", false, false, PresenceHeld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PresenceOf(t.Context(), st, target, fixedProbe(tc.alive, tc.known))
			if err != nil {
				t.Fatalf("PresenceOf: %v", err)
			}
			if got != tc.want {
				t.Errorf("PresenceOf with probe (%t, %t) = %v, want %v",
					tc.alive, tc.known, got, tc.want)
			}
		})
	}
}

// TestPresenceOfHandlesAnAliasWithNoHolder covers a state no code path
// produces.
//
// It is built with direct SQL because nothing else can build it: both alias
// INSERTs take session_id from the sessions primary key, ClaimAlias only writes
// a non-empty session, and nothing sets the column to NULL or deletes a session
// row. This test is NOT evidence that the state occurs — it is here because
// PresenceOf has nothing to probe when a target carries no session id, so the
// branch has to exist and therefore has to be defined.
func TestPresenceOfHandlesAnAliasWithNoHolder(t *testing.T) {
	st, _ := newStore(t)
	orphanAlias(t, st, "stranded")

	// A target with no session id at all, and one naming a session row that is
	// not there. Both are the same absence.
	for _, target := range []Target{
		{Alias: "stranded"},
		{Alias: "stranded", SessionID: "never-recorded"},
	} {
		got, err := PresenceOf(t.Context(), st, target, fixedProbe(true, true))
		if err != nil {
			t.Fatalf("PresenceOf(%+v): %v", target, err)
		}
		if got != PresenceUnheld {
			t.Errorf("PresenceOf(%+v) = %v, want PresenceUnheld", target, got)
		}
	}
	if note := PresenceUnheld.Note("stranded"); note == "" {
		t.Error("PresenceUnheld has no note, so a sender would be told nothing")
	}
}

// TestPresenceNotesArePinned holds the sentences a sender acts on.
//
// Pinned rather than screened, on the reasoning wake's Describe pin records: no
// test can read a sentence for its meaning, and a keyword screen passes a
// reword that has quietly lost one. The stranded line is the whole point of
// this work — it replaces "it will be delivered on its next activity", which
// was true of a queue and false of a reader, and read as reassurance at exactly
// the moment nobody was there.
func TestPresenceNotesArePinned(t *testing.T) {
	const wantStranded = "note: suzu's session is gone — the message waits, but nothing will read it " +
		"until a session in that workspace claims the name; pager who lists who can answer now"
	const wantUnheld = "note: nothing holds suzu — the message waits until a session claims that name"

	if got := PresenceStranded.Note("suzu"); got != wantStranded {
		t.Errorf("PresenceStranded.Note = %q\nwant %q\n"+
			"Reword freely, but only to something that still says all three: nobody is behind "+
			"the name, the message is kept, and a claim from that workspace is how it gets read.",
			got, wantStranded)
	}
	if got := PresenceUnheld.Note("suzu"); got != wantUnheld {
		t.Errorf("PresenceUnheld.Note = %q\nwant %q", got, wantUnheld)
	}
	// Held stays silent so that a reader is never handed two verdicts about one
	// recipient — wake.Describe is what speaks there.
	if got := PresenceHeld.Note("suzu"); got != "" {
		t.Errorf("PresenceHeld.Note = %q, want \"\" — wake speaks for a held target", got)
	}
}

// TestPresenceNoteDistinguishesItsTwoCases guards the one way this could be
// written and still pass the pin: two branches that say the same thing.
//
// The states are different facts and a sender acts on them differently. Unheld
// means the name was never taken, so any session may claim it. Stranded means
// it was taken and the holder died, so only that holder's workspace can.
func TestPresenceNoteDistinguishesItsTwoCases(t *testing.T) {
	unheld, stranded := PresenceUnheld.Note("suzu"), PresenceStranded.Note("suzu")
	if unheld == stranded {
		t.Fatal("the two notes are identical, so a sender cannot tell the cases apart")
	}
	if !strings.Contains(stranded, "session is gone") {
		t.Error("the stranded note does not say the holder's session is gone")
	}
	if strings.Contains(unheld, "gone") {
		t.Error("the unheld note claims something is gone; nothing ever held that name")
	}
}
