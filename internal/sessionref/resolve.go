// Package sessionref resolves which session a pager invocation belongs to.
//
// The CLI, the MCP server, and the hooks all resolve through this one type, so
// a message can never be attributed by one rule on one path and a different
// rule on another. An earlier design described only the MCP path, which would
// have left `pager send` — the primary sending interface — refused by pager's
// own policy.
//
// The tiers, and why detection is trusted at all, are in
// docs/session-binding.md.
package sessionref

import (
	"context"
	"os"
	"strings"
)

// Source records which tier produced a Ref.
type Source string

const (
	SourceFlag         Source = "flag"         // --session
	SourceEnv          Source = "env"          // PAGER_SESSION
	SourceHost         Source = "host"         // host detection → recorded session
	SourceUnattributed Source = "unattributed" // nothing resolved
)

// Ref is the outcome of a resolution.
type Ref struct {
	SessionID string
	Source    Source

	// Client and Host carry the detected host process, set whenever detection
	// succeeded — including when an earlier tier supplied the session id. A
	// hook needs both: it learns its session from its own stdin and uses these
	// to record that session against the host every other path looks up.
	Client string
	Host   Instance
}

// Attributed reports whether a session was resolved. An unattributed send is
// refused unless the caller asserts --human.
func (r Ref) Attributed() bool { return r.SessionID != "" }

// LookupFunc returns the session recorded against a host key, or "" when no
// live session holds it. In production this is backed by store.SessionByHost.
type LookupFunc func(ctx context.Context, client string, host Instance) (string, error)

// Resolver resolves session references. Use New; the zero value is not usable.
type Resolver struct {
	getenv func(string) string
	detect func() (string, Instance, bool)
	lookup LookupFunc
}

// New returns a Resolver wired to the real environment and host detection.
func New(lookup LookupFunc) *Resolver {
	return &Resolver{getenv: os.Getenv, detect: Detect, lookup: lookup}
}

// Resolve applies the four tiers in order, where flag is the --session value
// and is empty when it was not given.
//
// Failing to resolve is not an error: it yields SourceUnattributed and lets the
// caller decide. Only a lookup that genuinely failed returns an error, so a
// broken database is never mistaken for "no session".
//
// Detection runs first even when a flag or the environment will answer, because
// the hook path needs the host key regardless. It costs one walk up the process
// ancestry.
func (r *Resolver) Resolve(ctx context.Context, flag string) (Ref, error) {
	ref := Ref{Source: SourceUnattributed}
	client, host, detected := r.detect()
	if detected {
		ref.Client, ref.Host = client, host
	}

	if id := strings.TrimSpace(flag); id != "" {
		ref.SessionID, ref.Source = id, SourceFlag
		return ref, nil
	}
	if id := strings.TrimSpace(r.getenv("PAGER_SESSION")); id != "" {
		ref.SessionID, ref.Source = id, SourceEnv
		return ref, nil
	}
	if !detected || r.lookup == nil {
		return ref, nil
	}

	id, err := r.lookup(ctx, client, host)
	if err != nil {
		return ref, err
	}
	if id != "" {
		ref.SessionID, ref.Source = id, SourceHost
	}
	return ref, nil
}
