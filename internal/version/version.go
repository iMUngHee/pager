// Package version names this build of pager.
package version

import "runtime/debug"

// Version is pager's release version: what `pager version` prints, and what
// the MCP server and the Codex app-server client report. Every bump comes with
// a `## <version>` entry at the top of CHANGELOG.md, in the same commit;
// TestChangelogHasCurrentVersion fails without it.
const Version = "0.7.1"

// String is Version followed by the build it came from, when the toolchain
// recorded one: the commit of a checkout build, or the module version that
// `go install …@latest` resolved.
func String() string {
	if build := buildRef(); build != "" {
		return Version + " (" + build + ")"
	}
	return Version
}

func buildRef() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		if dirty {
			rev += "-dirty"
		}
		return rev
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return ""
}
