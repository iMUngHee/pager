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
// in milliseconds; linux: starttime in clock ticks). It is only ever compared
// for equality between processes on the same machine, so its unit does not
// matter — only its stability does. Its job is to make a recycled pid a miss
// rather than a misattribution.
type Instance struct {
	Pid   int
	Start int64
}

// Valid reports whether the instance names a real, non-root process.
func (i Instance) Valid() bool { return i.Pid > 1 }

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
func Detect() (string, Instance, bool) {
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
		ppid, start, cmd, ok := procInfo(pid)
		if !ok {
			return Unknown, Instance{}, false
		}
		for _, client := range candidates {
			if commandMatchesClient(client, cmd) {
				return client, Instance{Pid: pid, Start: start}, true
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
