package sessionref

import (
	"context"
	"errors"
	"testing"
)

// fixedHost is an arbitrary detected host used by the tier tests.
var fixedHost = Instance{Pid: 4242, Start: 987654321}

// newTestResolver builds a Resolver with every impure input faked, so the tier
// ordering can be exercised without a process tree or a database.
func newTestResolver(env map[string]string, detected bool, lookup LookupFunc) *Resolver {
	return &Resolver{
		getenv: func(k string) string { return env[k] },
		detect: func() (string, Instance, bool) {
			if !detected {
				return Unknown, Instance{}, false
			}
			return Claude, fixedHost, true
		},
		lookup: lookup,
	}
}

func lookupReturning(id string) LookupFunc {
	return func(context.Context, string, Instance) (string, error) { return id, nil }
}

// TestResolveTierOrder walks the full precedence table. Both the CLI and the
// MCP server call this same method, so one table covers both.
func TestResolveTierOrder(t *testing.T) {
	tests := []struct {
		name       string
		flag       string
		env        map[string]string
		detected   bool
		lookup     LookupFunc
		wantID     string
		wantSource Source
	}{
		{
			name: "tier 1 flag wins over everything",
			flag: "from-flag",
			env:  map[string]string{"PAGER_SESSION": "from-env"},

			detected: true, lookup: lookupReturning("from-host"),
			wantID: "from-flag", wantSource: SourceFlag,
		},
		{
			name: "tier 2 env wins when no flag",
			env:  map[string]string{"PAGER_SESSION": "from-env"},

			detected: true, lookup: lookupReturning("from-host"),
			wantID: "from-env", wantSource: SourceEnv,
		},
		{
			name:     "tier 3 host detection when neither flag nor env",
			detected: true, lookup: lookupReturning("from-host"),
			wantID: "from-host", wantSource: SourceHost,
		},
		{
			name:     "tier 4 unattributed when detection fails",
			detected: false, lookup: lookupReturning("from-host"),
			wantID: "", wantSource: SourceUnattributed,
		},
		{
			name:     "tier 4 unattributed when host holds no session",
			detected: true, lookup: lookupReturning(""),
			wantID: "", wantSource: SourceUnattributed,
		},
		{
			name: "blank flag falls through rather than resolving to empty",
			flag: "   ",
			env:  map[string]string{"PAGER_SESSION": "from-env"},

			detected: true, lookup: lookupReturning("from-host"),
			wantID: "from-env", wantSource: SourceEnv,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := newTestResolver(tc.env, tc.detected, tc.lookup).Resolve(t.Context(), tc.flag)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if ref.SessionID != tc.wantID {
				t.Errorf("SessionID = %q, want %q", ref.SessionID, tc.wantID)
			}
			if ref.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", ref.Source, tc.wantSource)
			}
			if got := ref.Attributed(); got != (tc.wantID != "") {
				t.Errorf("Attributed() = %v, want %v", got, tc.wantID != "")
			}
		})
	}
}

// TestResolveCarriesHostWhenEarlierTierWins is what the hook depends on: it
// learns its session id from its own stdin (tier 1) and still needs the host
// key to record that session for everyone else to find.
func TestResolveCarriesHostWhenEarlierTierWins(t *testing.T) {
	r := newTestResolver(nil, true, lookupReturning("unused"))
	ref, err := r.Resolve(t.Context(), "from-flag")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ref.Client != Claude || ref.Host != fixedHost {
		t.Errorf("host context = (%q, %+v), want (%q, %+v)", ref.Client, ref.Host, Claude, fixedHost)
	}
}

// TestResolveLookupErrorIsNotSilentlyUnattributed keeps a broken database from
// looking like "no session": the first would refuse the send with a diagnosable
// error, the second would refuse it as though the caller were anonymous.
func TestResolveLookupErrorIsNotSilentlyUnattributed(t *testing.T) {
	want := errors.New("database is gone")
	r := newTestResolver(nil, true, func(context.Context, string, Instance) (string, error) {
		return "", want
	})
	ref, err := r.Resolve(t.Context(), "")
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if ref.Attributed() {
		t.Error("a failed lookup produced an attributed ref")
	}
}

// TestResolveSkipsLookupWhenDetectionFails guards the fail-closed direction: no
// host means no query, not a query with a zero key that might match a row.
func TestResolveSkipsLookupWhenDetectionFails(t *testing.T) {
	called := false
	r := newTestResolver(nil, false, func(context.Context, string, Instance) (string, error) {
		called = true
		return "somebody", nil
	})
	if _, err := r.Resolve(t.Context(), ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if called {
		t.Error("lookup ran without a detected host")
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"claude": Claude, "Claude": Claude, "claude-code": Claude, "claude_code": Claude,
		"codex": Codex, " CODEX ": Codex, "codex-cli": Codex, "codex_cli": Codex,
		"": Unknown, "cursor": Unknown, "claudia": Unknown,
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCommandMatchesClient pins the matching rule, including the reason only
// comm and argv0 are ever passed in: a hook command line mentioning ~/.codex
// must not make an unrelated process look like a codex host.
func TestCommandMatchesClient(t *testing.T) {
	tests := []struct {
		client, cmd string
		want        bool
	}{
		{Claude, "claude", true},
		{Claude, "claude /opt/homebrew/bin/claude", true},
		{Claude, "node /usr/local/bin/claude", true},
		{Claude, "claude-code /usr/bin/claude-code", true},
		{Claude, "claude_code", true},
		{Claude, "CLAUDE", true},
		{Codex, "codex /usr/local/bin/codex", true},

		{Claude, "zsh /bin/zsh", false},
		{Claude, "claudia /usr/bin/claudia", false},
		{Codex, "sh /Users/someone/.codex/hooks/run.sh", false},
		{Codex, "sh -c pager hook Stop --config ~/.codex/config.toml", false},
		{Claude, "", false},
	}
	for _, tc := range tests {
		if got := commandMatchesClient(tc.client, tc.cmd); got != tc.want {
			t.Errorf("commandMatchesClient(%q, %q) = %v, want %v", tc.client, tc.cmd, got, tc.want)
		}
	}
}

func TestInstanceValid(t *testing.T) {
	for _, tc := range []struct {
		inst Instance
		want bool
	}{
		{Instance{Pid: 4242, Start: 1}, true},
		{Instance{}, false},
		{Instance{Pid: 1}, false}, // init is never a host
		{Instance{Pid: -1}, false},
	} {
		if got := tc.inst.Valid(); got != tc.want {
			t.Errorf("Instance%+v.Valid() = %v, want %v", tc.inst, got, tc.want)
		}
	}
}

// TestDetectPinnedToUnknownClientFailsClosed: naming a client pager does not
// know must mean no host, not any host. Scripts rely on it to declare
// themselves outside a session.
func TestDetectPinnedToUnknownClientFailsClosed(t *testing.T) {
	t.Setenv("PAGER_CLIENT", "none")
	client, host, ok := Detect()
	if ok || client != Unknown || host.Valid() {
		t.Errorf("Detect() = (%q, %+v, %v), want no host", client, host, ok)
	}
}

// TestDetectPinnedToAbsentClient exercises the real ancestry walk in the
// failing direction. The test binary's ancestors are not named "codex", so
// detection must terminate and report failure rather than guess.
func TestDetectPinnedToAbsentClient(t *testing.T) {
	t.Setenv("PAGER_CLIENT", "codex")
	client, host, ok := Detect()
	if ok {
		t.Fatalf("detected a codex host from the test binary: %q %+v", client, host)
	}
	if client != Unknown {
		t.Errorf("client = %q, want %q", client, Unknown)
	}
	if host.Valid() {
		t.Errorf("host = %+v, want invalid", host)
	}
}
