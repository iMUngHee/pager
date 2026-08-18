package main

import (
	"flag"
	"strings"
)

// permute reorders args so that a flag written after a positional is still
// parsed, which is what every usage string in this package advertises.
//
// Go's flag package stops at the first non-flag argument, so `send <target>
// <body> --human` never parsed --human at all: it stayed a positional and was
// joined into the message body. That failure was silent in the worst way — the
// flag it swallowed is an operator assertion, so the send was recorded as
// ordinary agent traffic, counted against the automatic budget, and subject to
// the causal hop limit, while the person who typed it saw a message go out.
// The same stop made `alias <name> --session <id>` fail its own usage check,
// because the unparsed flag and its value stayed behind as extra positionals.
//
// The result is flags first, then a literal "--", then the positionals.
//
// Only the tail is treated differently from before. Up to the first positional
// this hands flag.Parse exactly what it would have received, so an undefined
// flag written there is still the error it has always been; past that point a
// dash-word is only pulled out when it names a flag this command declares.
func permute(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	seenPositional := false

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// A literal "--" ends the caller's flags. Everything after it is a
		// positional no matter what it looks like, so stop extracting.
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}

		name, hasValue := flagName(arg)
		if name == "" {
			positional = append(positional, arg)
			seenPositional = true
			continue
		}

		// Past the first positional, a dash-word this command does not declare
		// stays a positional. A message body is free text and may well contain
		// one; treating it as an unknown flag would reject sends that work
		// today. Before the first positional there is no body to belong to, so
		// it goes to flag.Parse and is reported the way it always was.
		f := fs.Lookup(name)
		if f == nil && seenPositional {
			positional = append(positional, arg)
			continue
		}

		flags = append(flags, arg)
		// A flag that takes a value and was not given one inline consumes the
		// next argument. Booleans never do: `--human` is complete on its own,
		// and only the `--human=false` spelling carries a value. An undefined
		// flag consumes nothing — flag.Parse is about to reject it, and
		// guessing its arity could swallow a positional on the way out.
		if f != nil && !hasValue && !isBoolFlag(f) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}

	if len(positional) == 0 {
		return flags
	}
	// The separator is what keeps a dash-leading body word from being parsed
	// as a flag once it sits ahead of no positional of its own.
	return append(append(flags, "--"), positional...)
}

// flagName returns the flag name in arg and whether it carried an inline
// value, or "" when arg is not written as a flag at all.
//
// Both -name and --name are accepted because Go's flag package accepts both,
// and a caller who learned one spelling should not find it works only when
// written before the positionals.
func flagName(arg string) (name string, hasValue bool) {
	if len(arg) < 2 || arg[0] != '-' {
		return "", false
	}
	trimmed := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	// "-" alone, or "---", is not a flag.
	if trimmed == "" || strings.HasPrefix(trimmed, "-") {
		return "", false
	}
	if name, _, found := strings.Cut(trimmed, "="); found {
		return name, true
	}
	return trimmed, false
}

// isBoolFlag reports whether a flag is satisfied by its presence alone, using
// the same optional interface flag.Parse itself checks.
func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
