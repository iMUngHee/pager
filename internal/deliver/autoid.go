package deliver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/iMUngHee/pager/internal/store"
)

// A generated name alternates consonant and vowel, so it can be said out loud
// and typed back from hearing it once. The grammar also settles what a name can
// never be: "human" is consonant-vowel-consonant-vowel-consonant, so no
// generated name can collide with the label that means a person, and no list of
// reserved words is needed to keep them apart.
const (
	autoConsonants = "bcdfghjklmnprstvwz" // 18
	autoVowels     = "aeiou"              // 5
)

// Two syllables give 8100 names, which is plenty while a person has a handful
// of sessions open. Names are never reclaimed, though, so the space fills over
// years rather than never — after autoShortAttempts consecutive collisions the
// generator moves to three syllables (729,000). That escalation is the reason
// assignment cannot quietly give up and leave a session showing a UUID, which
// is the symptom this whole feature exists to remove.
const (
	autoShortSyllables = 2
	autoLongSyllables  = 3
	autoShortAttempts  = 8
	autoAttempts       = 12
)

// EnsureAutoAlias gives a session a short, meaningless name when it has none.
//
// It reports the name the session holds afterwards, and whether this call is
// the one that assigned it. That second value is what lets a hook introduce the
// name once without keeping any state of its own.
//
// A session whose workspace is not known yet is left unnamed and reported as
// such, without an error: see assignAutoName.
func EnsureAutoAlias(ctx context.Context, st *store.Store, session string) (string, bool, error) {
	return ensureAutoAlias(ctx, st, session, generateAutoName)
}

// ensureAutoAlias is EnsureAutoAlias with the generator injected, so a test can
// decide which candidates are offered and in what order.
func ensureAutoAlias(ctx context.Context, st *store.Store, session string, gen func(syllables int) (string, error)) (string, bool, error) {
	if session == "" {
		return "", false, errors.New("ensure auto alias: empty session id")
	}

	// The common case by far: the session was named on an earlier event and
	// this call only has to say so. Everything below runs once per session.
	held, err := PrimaryAlias(ctx, st, session)
	if err != nil {
		return "", false, err
	}
	if held != "" {
		return held, false, nil
	}
	known, err := workspaceKnown(ctx, st, session)
	if err != nil || !known {
		return "", false, err
	}

	for attempt := range autoAttempts {
		syllables := autoShortSyllables
		if attempt >= autoShortAttempts {
			syllables = autoLongSyllables
		}
		candidate, err := gen(syllables)
		if err != nil {
			return "", false, err
		}
		assigned, err := assignAutoName(ctx, st, candidate, session)
		if err != nil {
			return "", false, err
		}
		if assigned {
			return candidate, true, nil
		}
		// Zero rows means either the name was taken or another hook named
		// this session first. Only a second read tells the two apart, and the
		// second is a success for the caller — it just was not the assigner.
		held, err := PrimaryAlias(ctx, st, session)
		if err != nil {
			return "", false, err
		}
		if held != "" {
			return held, false, nil
		}
	}
	return "", false, fmt.Errorf("no free name for session %s after %d attempts", session, autoAttempts)
}

// workspaceKnown reports whether the stored session row carries the root and
// tool that an alias copies at insertion time.
//
// It reads the stored row rather than whatever the caller happens to hold. A
// hook whose host detection just failed passes an empty tool, but RecordSession
// leaves an already-known value in place, so such a session may well still be
// eligible on the strength of what an earlier call recorded. Judging from the
// caller's own record instead would skip it forever.
func workspaceKnown(ctx context.Context, st *store.Store, session string) (bool, error) {
	var known bool
	err := st.DB().QueryRowContext(ctx,
		"SELECT root <> '' AND tool <> '' FROM sessions WHERE session_id = ?", session).Scan(&known)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read session workspace: %w", err)
	}
	return known, nil
}

// assignAutoName inserts one candidate name, reporting whether it took.
//
// Every precondition is a condition of this one statement: the session exists,
// it knows its workspace, and it holds no name yet. Checking the last one
// before the insert would leave a window in which two hooks for the same
// session each decide they are first, and the session would end up wearing two
// names — with the label rule then picking between them by timestamp.
//
// A name already in use fails the uniqueness constraint rather than this WHERE
// clause, which is why the two outcomes are indistinguishable here and the
// caller re-reads to tell them apart.
func assignAutoName(ctx context.Context, st *store.Store, name, session string) (bool, error) {
	res, err := st.Exec(ctx, `
		INSERT INTO aliases(alias, session_id, root, tool, updated_at)
		SELECT ?, s.session_id, s.root, s.tool, ?
		  FROM sessions s
		 WHERE s.session_id = ?
		   AND s.root <> '' AND s.tool <> ''
		   AND NOT EXISTS (SELECT 1 FROM aliases a WHERE a.session_id = s.session_id)
		ON CONFLICT(alias) DO NOTHING`,
		name, st.Now(), session)
	if err != nil {
		return false, fmt.Errorf("assign auto name: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("assign auto name: %w", err)
	}
	return n > 0, nil
}

// generateAutoName returns one candidate of the requested syllable count.
func generateAutoName(syllables int) (string, error) {
	var sb strings.Builder
	sb.Grow(syllables * 2)
	for range syllables {
		consonant, err := pickByte(autoConsonants)
		if err != nil {
			return "", err
		}
		vowel, err := pickByte(autoVowels)
		if err != nil {
			return "", err
		}
		sb.WriteByte(consonant)
		sb.WriteByte(vowel)
	}
	return sb.String(), nil
}

// pickByte chooses one byte of set uniformly.
//
// crypto/rand.Int rather than a byte modulo: 256 is not a multiple of 18, so
// the modulo would hand out the first four consonants slightly more often. A
// namespace this small, never reclaimed, cannot afford a skew that makes
// collisions arrive sooner than the arithmetic says they should.
func pickByte(set string) (byte, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
	if err != nil {
		return 0, fmt.Errorf("generate name: %w", err)
	}
	return set[n.Int64()], nil
}
