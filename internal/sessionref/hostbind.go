package sessionref

import (
	"os"
	"path/filepath"
	"strings"
)

// Host tool labels.
const (
	Unknown = "unknown"
	Claude  = "claude"
	Codex   = "codex"
)

// maxWalkDepth bounds the ancestry walk. A hook or a CLI invocation reaches its
// host through at most a couple of shell wrappers, so 16 is generous.
const maxWalkDepth = 16

// Instance identifies a host process.
//
// Start is an opaque, platform-relative process-start token (darwin: start time
// in milliseconds; linux: starttime in clock ticks; windows: creation time in
// nanoseconds). It is only ever compared for equality between processes on the
// same machine, so its unit does not matter — only its stability does. Its job
// is to make a recycled pid a miss rather than a misattribution.
//
// Zero means the token could not be read, which windows can answer for a
// process this user may not open. It is never a real token, so it must never be
// stored as one: two different processes that both refuse to be opened would
// compare equal on it, and the recycled pid this field exists to catch would
// read as the original still running.
type Instance struct {
	Pid   int
	Start int64
}

// Valid reports whether the instance names a real, non-root process with a
// usable identity.
//
// A pid alone is not an identity — see Start. An instance carrying a pid but no
// token names a process nobody can distinguish from the next one to hold that
// number, so callers treat it as no host at all rather than as a weaker one.
func (i Instance) Valid() bool { return i.Pid > 1 && i.Start != 0 }

// Alive reports whether inst still names the process it was recorded for, and
// whether this platform can answer the question at all.
//
// The start token is what makes this worth doing here rather than in a caller:
// `kill -0 <pid>` says only that some process holds the number, so a recycled
// pid reads as the original still running. Comparing the token is the check
// Instance's doc comment says the field exists for.
//
// known is the difference between "no" and "cannot tell", and the two must not
// collapse. A caller acts on them differently — mail addressed to an inbox whose
// process is definitely gone is stranded, while mail whose process cannot be
// checked is merely unverified, and hiding the second on the strength of the
// first drops notifications nobody has abandoned. An instance that was never
// detected (Valid is false) is unknowable for the same reason: there is no
// identity to ask about.
func Alive(inst Instance) (alive, known bool) {
	if !inst.Valid() || !procInfoSupported {
		return false, false
	}
	start, exists, known := newProcSource().startToken(inst.Pid)
	switch {
	case !known:
		// The platform could not answer — the process may or may not be
		// there. Saying "gone" here would strand mail nobody abandoned.
		return false, false
	case !exists:
		// The platform can answer and its answer is that no such process
		// exists. That is a definite no, not an absence of information.
		return false, true
	}
	return start == inst.Start, true
}

// AliveAt is Alive for a caller holding the two columns rather than an Instance.
//
// The store keeps host_pid and host_start as separate values, so every caller
// reading a session out of the database has the pair and not the struct. This
// exists so they do not each rebuild it: deliver.HostProbe is written in terms
// of the pair precisely so that deliver never has to name a sessionref type.
func AliveAt(pid int, start int64) (alive, known bool) {
	return Alive(Instance{Pid: pid, Start: start})
}

// Normalize maps the accepted spellings of a host label onto the stable one.
func Normalize(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case Claude, "claude-code", "claude_code":
		return Claude
	case Codex, "codex-cli", "codex_cli":
		return Codex
	default:
		return Unknown
	}
}

// Detect resolves the host tool label and the host process in a single walk up
// the ancestry from this process.
//
// The hook, the CLI the agent runs through its Bash tool, and the MCP server
// are all descendants of the same host, so all three converge on the same
// Instance — which is what lets one of them record a session and the others
// look it up. See docs/session-binding.md.
//
// PAGER_CLIENT pins the label when it is set; otherwise either known client is
// accepted. PAGER_ is not a CONTEXT_ prefix, so it survives Claude Code's
// environment scrub.
//
// Setting it to something unrecognised means no host, not "any host". An
// operator who names a client and gets a different one silently attributed is
// worse served than one whose send is refused, and it gives a script a way to
// say it is deliberately outside any session.
//
// One source serves the whole walk: a platform that can only answer about a
// parent by reading the entire process table reads it once, not once per level.
func Detect() (string, Instance, bool) {
	return detectFrom(newProcSource())
}

// procWalker is the part of a procSource the ancestry walk uses.
//
// detectFrom takes it rather than calling the platform directly so a test can
// hand it an ancestry no real machine produces on demand — a host whose start
// token is unreadable, most of all, which is the branch that decides between
// refusing a session and misattributing one.
type procWalker interface {
	info(pid int) (ppid int, start int64, cmd string, ok bool)
}

func detectFrom(src procWalker) (string, Instance, bool) {
	candidates := []string{Claude, Codex}
	if raw := strings.TrimSpace(os.Getenv("PAGER_CLIENT")); raw != "" {
		pinned := Normalize(raw)
		if pinned == Unknown {
			return Unknown, Instance{}, false
		}
		candidates = []string{pinned}
	}

	pid := os.Getppid()
	for depth := 0; pid > 1 && depth < maxWalkDepth; depth++ {
		ppid, start, cmd, ok := src.info(pid)
		if !ok {
			return Unknown, Instance{}, false
		}
		for _, client := range candidates {
			if commandMatchesClient(client, cmd) {
				inst := Instance{Pid: pid, Start: start}
				if !inst.Valid() {
					// The host is right there but its start token is
					// unreadable, so it cannot be told apart from the next
					// process to hold that pid. Refusing is the same answer
					// this contract gives an undetectable host: --session and
					// PAGER_SESSION still work, and a send with neither is
					// refused rather than misattributed.
					return Unknown, Instance{}, false
				}
				return client, inst, true
			}
		}
		if ppid == pid { // defensive: never loop on a self-parenting pid
			break
		}
		pid = ppid
	}
	return Unknown, Instance{}, false
}

// commandMatchesClient reports whether cmd identifies the given client.
//
// Only comm and argv0 are ever passed in, never the full argv: a hook command
// line can contain paths like ~/.codex, and matching against those would make
// an unrelated process look like a codex host.
func commandMatchesClient(client, cmd string) bool {
	for field := range strings.FieldsSeq(strings.ToLower(cmd)) {
		base := strings.TrimSuffix(filepath.Base(field), ".exe")
		if base == client || strings.HasPrefix(base, client+"-") || strings.HasPrefix(base, client+"_") {
			return true
		}
	}
	return false
}
