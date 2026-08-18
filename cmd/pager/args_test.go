package main

import (
	"flag"
	"io"
	"slices"
	"testing"
)

// sendLikeFlags mirrors the shape that made this necessary: one boolean and two
// value-taking flags, sitting behind two positionals.
func sendLikeFlags() (*flag.FlagSet, *bool, *string, *string) {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	human := fs.Bool("human", false, "")
	session := fs.String("session", "", "")
	label := fs.String("label", "", "")
	return fs, human, session, label
}

// TestPermuteParsesFlagsAfterPositionals is the whole point: the invocation the
// usage string advertises has to work.
func TestPermuteParsesFlagsAfterPositionals(t *testing.T) {
	fs, human, session, label := sendLikeFlags()
	if err := fs.Parse(permute(fs, []string{"faro", "hello there", "--human", "--session", "s1", "--label", "me"})); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*human {
		t.Error("--human written after the positionals was not parsed")
	}
	if *session != "s1" {
		t.Errorf("session = %q, want %q", *session, "s1")
	}
	if *label != "me" {
		t.Errorf("label = %q, want %q", *label, "me")
	}
	if got := fs.Args(); !slices.Equal(got, []string{"faro", "hello there"}) {
		t.Errorf("positionals = %v, want the target and body only", got)
	}
}

// TestPermuteConsumesFlagValues fixes the rule that separates the two kinds of
// flag. A value-taking flag written without "=" owns the token after it, and
// mistaking that token for a positional would put a session id in the body.
func TestPermuteConsumesFlagValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"separate value", []string{"faro", "body", "--session", "s1"}},
		{"inline value", []string{"faro", "body", "--session=s1"}},
		{"single dash", []string{"faro", "body", "-session", "s1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, _, session, _ := sendLikeFlags()
			if err := fs.Parse(permute(fs, tc.args)); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if *session != "s1" {
				t.Errorf("session = %q, want %q", *session, "s1")
			}
			if got := fs.Args(); !slices.Equal(got, []string{"faro", "body"}) {
				t.Errorf("positionals = %v, want the target and body only", got)
			}
		})
	}
}

// TestPermuteLeavesUnknownDashTokens is the limit that keeps this from breaking
// sends that work today. A body is free text, so a word starting with a dash is
// a word — not a flag this command failed to declare.
func TestPermuteLeavesUnknownDashTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"unknown long flag", []string{"faro", "--not-a-flag"}, []string{"faro", "--not-a-flag"}},
		{"lone dash", []string{"faro", "-"}, []string{"faro", "-"}},
		{"negative number", []string{"faro", "-1"}, []string{"faro", "-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, _, _, _ := sendLikeFlags()
			if err := fs.Parse(permute(fs, tc.args)); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := fs.Args(); !slices.Equal(got, tc.want) {
				t.Errorf("positionals = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPermuteSeparatorEscapes covers the way out for the one case this change
// takes away: a body word that happens to spell a declared flag.
func TestPermuteSeparatorEscapes(t *testing.T) {
	fs, human, _, _ := sendLikeFlags()
	if err := fs.Parse(permute(fs, []string{"faro", "--", "--human", "is the flag"})); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *human {
		t.Error("--human after a literal -- was parsed as a flag")
	}
	if got := fs.Args(); !slices.Equal(got, []string{"faro", "--human", "is the flag"}) {
		t.Errorf("positionals = %v, want the escaped words kept verbatim", got)
	}
}

// TestPermuteLeavesLeadingFlagsAlone guards the invocation that already worked.
// Every existing caller writes flags first, and none of them may change.
func TestPermuteLeavesLeadingFlagsAlone(t *testing.T) {
	fs, human, session, _ := sendLikeFlags()
	if err := fs.Parse(permute(fs, []string{"--session", "s1", "--human", "faro", "body"})); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*human || *session != "s1" {
		t.Errorf("human = %v, session = %q, want true and %q", *human, *session, "s1")
	}
	if got := fs.Args(); !slices.Equal(got, []string{"faro", "body"}) {
		t.Errorf("positionals = %v, want the target and body only", got)
	}
}

// TestPermuteWithoutPositionals covers the subcommands that take none. They go
// through the same call, so their arguments must come out untouched.
func TestPermuteWithoutPositionals(t *testing.T) {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	expired := fs.Bool("expired", false, "")

	got := permute(fs, []string{"--expired"})
	if !slices.Equal(got, []string{"--expired"}) {
		t.Errorf("permuted = %v, want the arguments unchanged with no separator", got)
	}
	if err := fs.Parse(got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*expired {
		t.Error("--expired was not parsed")
	}
}

// TestPermuteKeepsUndefinedFlagErrors makes sure the reordering does not hide a
// real mistake: a flag written before the positionals is still the caller's
// error to see, as flag.Parse has always reported it.
func TestPermuteKeepsUndefinedFlagErrors(t *testing.T) {
	fs, _, _, _ := sendLikeFlags()
	if err := fs.Parse(permute(fs, []string{"--nope", "faro", "body"})); err == nil {
		t.Fatal("parse accepted an undefined flag written before the positionals")
	}
}
