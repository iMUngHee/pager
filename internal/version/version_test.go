package version

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// entries returns the versions CHANGELOG.md has entries for, in file order,
// and the body of the first one.
func entries(t *testing.T) ([]string, string) {
	t.Helper()
	raw, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	var versions []string
	var first strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if head, ok := strings.CutPrefix(line, "## "); ok {
			versions = append(versions, strings.Fields(head + " ")[0])
			continue
		}
		if len(versions) == 1 && strings.TrimSpace(line) != "" {
			first.WriteString(line)
		}
	}
	return versions, first.String()
}

// TestChangelogHasCurrentVersion is what makes patch notes mandatory: a bump of
// Version without its CHANGELOG.md entry fails `go test`, and so `make
// reinstall`, which runs the tests before it installs anything.
func TestChangelogHasCurrentVersion(t *testing.T) {
	versions, body := entries(t)
	if len(versions) == 0 || versions[0] != Version {
		t.Fatalf("CHANGELOG.md must open with `## %s — <date>` for version.Version (found %v): "+
			"write the patch notes in the commit that bumps the version", Version, versions)
	}
	if body == "" {
		t.Fatalf("the %s entry in CHANGELOG.md is empty", Version)
	}
}

func TestChangelogIsNewestFirst(t *testing.T) {
	versions, _ := entries(t)
	parse := func(v string) [3]int {
		var out [3]int
		parts := strings.Split(v, ".")
		if len(parts) != 3 {
			t.Fatalf("not a version: %q", v)
		}
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				t.Fatalf("not a version: %q", v)
			}
			out[i] = n
		}
		return out
	}
	newer := func(a, b [3]int) bool {
		for i := range a {
			if a[i] != b[i] {
				return a[i] > b[i]
			}
		}
		return false
	}
	for i := 1; i < len(versions); i++ {
		if !newer(parse(versions[i-1]), parse(versions[i])) {
			t.Errorf("CHANGELOG.md entries must be unique and newest first: %s then %s", versions[i-1], versions[i])
		}
	}
}

func TestStringStartsWithVersion(t *testing.T) {
	if s := String(); !strings.HasPrefix(s, Version) {
		t.Errorf("String() = %q, want it to start with %q", s, Version)
	}
}
