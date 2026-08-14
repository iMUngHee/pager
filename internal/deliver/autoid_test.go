package deliver

import (
	"context"
	"regexp"
	"sync"
	"testing"

	"github.com/unghee/pager/internal/store"
)

var (
	shortName = regexp.MustCompile(`^[bcdfghjklmnprstvwz][aeiou][bcdfghjklmnprstvwz][aeiou]$`)
	longName  = regexp.MustCompile(`^([bcdfghjklmnprstvwz][aeiou]){3}$`)
)

// recordSession registers a session with exactly the fields given, so a test
// can express "this session's host detection failed" as the empty tool the hook
// would actually write.
func recordSession(t *testing.T, st *store.Store, id, root, hostTool string) {
	t.Helper()
	if err := st.RecordSession(t.Context(), store.SessionRecord{ID: id, Tool: hostTool, Root: root}); err != nil {
		t.Fatalf("record session %s: %v", id, err)
	}
}

func aliasCount(t *testing.T, st *store.Store, session string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT count(*) FROM aliases WHERE session_id = ?", session).Scan(&n); err != nil {
		t.Fatalf("count aliases: %v", err)
	}
	return n
}

func TestAutoNameShape(t *testing.T) {
	for range 50 {
		short, err := generateAutoName(autoShortSyllables)
		if err != nil {
			t.Fatalf("generateAutoName: %v", err)
		}
		if !shortName.MatchString(short) {
			t.Fatalf("short name %q is not consonant-vowel-consonant-vowel", short)
		}
		long, err := generateAutoName(autoLongSyllables)
		if err != nil {
			t.Fatalf("generateAutoName: %v", err)
		}
		if !longName.MatchString(long) {
			t.Fatalf("long name %q is not three syllables", long)
		}
	}
}

// TestEnsureAutoAliasAssignsOnce fixes the contract the hook depends on: the
// call that assigns says so, and every later call reports the same name without
// claiming to have assigned it. Getting this wrong would either repeat the
// introduction on every event or never show it.
func TestEnsureAutoAliasAssignsOnce(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "s1", workspace, "")

	name, assigned, err := EnsureAutoAlias(t.Context(), st, "s1")
	if err != nil {
		t.Fatalf("EnsureAutoAlias: %v", err)
	}
	if !assigned {
		t.Fatal("first call did not report the assignment")
	}
	if !shortName.MatchString(name) {
		t.Fatalf("name = %q, want a four-letter name", name)
	}

	again, assigned, err := EnsureAutoAlias(t.Context(), st, "s1")
	if err != nil {
		t.Fatalf("EnsureAutoAlias: %v", err)
	}
	if assigned {
		t.Error("second call claimed to have assigned a name")
	}
	if again != name {
		t.Errorf("name = %q, want the already-held %q", again, name)
	}
	if n := aliasCount(t, st, "s1"); n != 1 {
		t.Errorf("alias rows = %d, want 1", n)
	}
}

// TestEnsureAutoAliasConcurrent uses a barrier rather than hoping the goroutines
// overlap. Two hooks for one session can genuinely run at once — SessionStart
// and UserPromptSubmit arrive together on a resume — and if both got past the
// "does it have a name" check the session would end up wearing two.
func TestEnsureAutoAliasConcurrent(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "s1", workspace, "")

	const runners = 4
	start := make(chan struct{})
	names := make([]string, runners)
	assigns := make([]bool, runners)
	errs := make([]error, runners)
	var wg sync.WaitGroup
	for r := range runners {
		wg.Go(func() {
			<-start
			names[r], assigns[r], errs[r] = EnsureAutoAlias(context.Background(), st, "s1")
		})
	}
	close(start)
	wg.Wait()

	assigned := 0
	for r := range runners {
		if errs[r] != nil {
			t.Fatalf("runner %d: %v", r, errs[r])
		}
		if assigns[r] {
			assigned++
		}
		if names[r] != names[0] {
			t.Errorf("runner %d saw %q, runner 0 saw %q — they must agree", r, names[r], names[0])
		}
	}
	if assigned != 1 {
		t.Errorf("%d runners reported assigning the name, want exactly 1", assigned)
	}
	if n := aliasCount(t, st, "s1"); n != 1 {
		t.Errorf("alias rows = %d, want 1", n)
	}
}

// TestAutoNameEscalatesAfterEight pins the escape hatch. Without it a crowded
// namespace makes assignment fail silently and sessions go back to showing
// UUIDs — the original bug, returning years later.
func TestAutoNameEscalatesAfterEight(t *testing.T) {
	st, _ := newStore(t)
	addSession(t, st, "s1", workspace, "")
	addSession(t, st, "holder", workspace, "")

	occupied := []string{"bavu", "miro", "zoka", "peti", "luna", "dako", "vemi", "sopu"}
	if len(occupied) != autoShortAttempts {
		t.Fatalf("test needs exactly %d occupied names, has %d", autoShortAttempts, len(occupied))
	}
	for _, name := range occupied {
		mustSetAlias(t, st, name, "holder")
	}

	var asked []int
	offered := 0
	gen := func(syllables int) (string, error) {
		asked = append(asked, syllables)
		if syllables == autoShortSyllables {
			if offered >= len(occupied) {
				t.Fatalf("generator asked for a %d-syllable name %d times, more than the occupied set", syllables, offered+1)
			}
			name := occupied[offered]
			offered++
			return name, nil
		}
		return "bavuko", nil
	}

	name, assigned, err := ensureAutoAlias(t.Context(), st, "s1", gen)
	if err != nil {
		t.Fatalf("ensureAutoAlias: %v", err)
	}
	if !assigned || name != "bavuko" {
		t.Fatalf("name = %q (assigned %v), want the escalated %q", name, assigned, "bavuko")
	}
	if len(asked) != autoShortAttempts+1 {
		t.Fatalf("generator was asked %d times, want %d", len(asked), autoShortAttempts+1)
	}
	for i, syllables := range asked[:autoShortAttempts] {
		if syllables != autoShortSyllables {
			t.Errorf("attempt %d asked for %d syllables, want %d", i+1, syllables, autoShortSyllables)
		}
	}
	if asked[autoShortAttempts] != autoLongSyllables {
		t.Errorf("attempt %d asked for %d syllables, want the escalation to %d",
			autoShortAttempts+1, asked[autoShortAttempts], autoLongSyllables)
	}
}

// TestNoNameWithoutTool and TestNoNameWithoutRoot cover the same rule from both
// sides: an alias copies root and tool at insertion and nothing updates them
// afterwards, so naming a session before its workspace is known would freeze a
// wrong workspace into the row — and that alias would then be invisible to
// orphan discovery and untransferable by claim.
func TestNoNameWithoutTool(t *testing.T) {
	st, _ := newStore(t)
	recordSession(t, st, "s1", workspace, "")

	name, assigned, err := EnsureAutoAlias(t.Context(), st, "s1")
	if err != nil {
		t.Fatalf("EnsureAutoAlias: %v", err)
	}
	if name != "" || assigned {
		t.Errorf("name = %q (assigned %v), want no name while the tool is unknown", name, assigned)
	}
	if n := aliasCount(t, st, "s1"); n != 0 {
		t.Errorf("alias rows = %d, want 0", n)
	}
}

func TestNoNameWithoutRoot(t *testing.T) {
	st, _ := newStore(t)
	recordSession(t, st, "s1", "", tool)

	name, assigned, err := EnsureAutoAlias(t.Context(), st, "s1")
	if err != nil {
		t.Fatalf("EnsureAutoAlias: %v", err)
	}
	if name != "" || assigned {
		t.Errorf("name = %q (assigned %v), want no name while the root is unknown", name, assigned)
	}
}

// TestNameAfterWorkspaceLearned is the reason eligibility is read from the
// stored row rather than from the caller's own record. A session enriched by
// pager attach stays eligible even when a later hook cannot detect its host,
// because RecordSession keeps what it already knew.
func TestNameAfterWorkspaceLearned(t *testing.T) {
	st, _ := newStore(t)
	recordSession(t, st, "s1", workspace, "")
	if _, assigned, err := EnsureAutoAlias(t.Context(), st, "s1"); err != nil || assigned {
		t.Fatalf("EnsureAutoAlias before the tool is known: assigned %v, err %v", assigned, err)
	}

	// attach fills in what the hook could not detect.
	recordSession(t, st, "s1", workspace, tool)
	// A later hook whose detection fails again writes an empty tool, which
	// must not undo eligibility.
	recordSession(t, st, "s1", workspace, "")

	name, assigned, err := EnsureAutoAlias(t.Context(), st, "s1")
	if err != nil {
		t.Fatalf("EnsureAutoAlias: %v", err)
	}
	if !assigned || !shortName.MatchString(name) {
		t.Fatalf("name = %q (assigned %v), want a name once the workspace is known", name, assigned)
	}
	var storedTool string
	if err := st.DB().QueryRowContext(t.Context(),
		"SELECT tool FROM aliases WHERE alias = ?", name).Scan(&storedTool); err != nil {
		t.Fatalf("read alias tool: %v", err)
	}
	if storedTool != tool {
		t.Errorf("alias tool = %q, want the known %q", storedTool, tool)
	}
}
