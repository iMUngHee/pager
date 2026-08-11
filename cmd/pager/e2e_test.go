package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These drive the real command dispatch end to end. They are the check that the
// pieces compose: a message left by one tool's session, in one repository,
// reaches another tool's session in a different repository.

// capture runs a command and returns its stdout.
func capture(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	go func() {
		_, _ = io.WriteString(inW, stdin)
		inW.Close()
	}()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = origIn, origOut }()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(outR)
		done <- string(b)
	}()

	runErr := run(args)
	outW.Close()
	inR.Close()
	return <-done, runErr
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := capture(t, "", args...)
	if err != nil {
		t.Fatalf("pager %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// TestE2ECrossWorkspaceDelivery is the product in one test: two repositories,
// two tools, no shared state between the projects themselves.
func TestE2ECrossWorkspaceDelivery(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	repoA := filepath.Join(dir, "repo-a")
	repoB := filepath.Join(dir, "repo-b")
	for _, d := range []string{repoA, repoB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// A Codex session in one repository.
	mustRun(t, "attach", "--session", "codex-1", "--tool", "codex", "--root", repoA)
	mustRun(t, "alias", "--session", "codex-1", "codex-box")

	// A Claude session in another.
	mustRun(t, "attach", "--session", "claude-1", "--tool", "claude", "--root", repoB)
	mustRun(t, "alias", "--session", "claude-1", "claude-box")

	sent := mustRun(t, "send", "--session", "codex-1", "claude-box", "the parser work is yours")
	if !strings.Contains(sent, "claude-box") {
		t.Fatalf("send did not confirm the target:\n%s", sent)
	}

	// The recipient's hook is what actually delivers it.
	payload := `{"session_id":"claude-1","cwd":"` + repoB + `","hook_event_name":"UserPromptSubmit","prompt":"what's next?"}`
	out, err := capture(t, payload, "hook", "UserPromptSubmit")
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if !strings.Contains(out, "the parser work is yours") {
		t.Fatalf("the message did not arrive:\n%s", out)
	}
	if !strings.Contains(out, "additionalContext") {
		t.Errorf("output is not the hook envelope:\n%s", out)
	}

	// And it is not delivered twice.
	again, err := capture(t, payload, "hook", "UserPromptSubmit")
	if err != nil {
		t.Fatalf("second hook: %v", err)
	}
	if again != "" {
		t.Errorf("the message was delivered again: %s", again)
	}

	// Neither repository holds any pager state; everything lives in the one
	// database. This is what "project-agnostic" has to mean in practice.
	for _, d := range []string{repoA, repoB} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatalf("read %s: %v", d, err)
		}
		if len(entries) != 0 {
			t.Errorf("%s gained %d entries, want none", d, len(entries))
		}
	}
}

// TestE2EReplyCarriesCausalDepth: the reply to a delivered message is not a
// fresh conversation, and the CLI reports the depth it was given.
func TestE2EReplyCarriesCausalDepth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "a", "--tool", "codex", "--root", dir)
	mustRun(t, "alias", "--session", "a", "a-box")
	mustRun(t, "attach", "--session", "b", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "b", "b-box")

	mustRun(t, "send", "--session", "a", "b-box", "look at this")

	payload := `{"session_id":"b","cwd":"` + dir + `","hook_event_name":"Stop"}`
	if _, err := capture(t, payload, "hook", "Stop"); err != nil {
		t.Fatalf("hook: %v", err)
	}

	reply := mustRun(t, "send", "--session", "b", "a-box", "on it")
	if !strings.Contains(reply, "hop 1") || !strings.Contains(reply, "caused") {
		t.Errorf("the reply did not inherit causal depth:\n%s", reply)
	}
}

// TestE2ESendWithoutContextIsRefused is the fail-closed default at the command
// line, and the escape hatch beside it.
func TestE2ESendWithoutContextIsRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "b", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "b", "b-box")

	// Stand outside any session, the way an independent shell does. Without
	// this the test would run inside a real host and attach would genuinely
	// have bound it, so the send would be correctly attributed.
	t.Setenv("PAGER_CLIENT", "none")

	if _, err := capture(t, "", "send", "b-box", "who am I"); err == nil {
		t.Error("an unattributed send succeeded, want it refused")
	}
	if _, err := capture(t, "", "send", "--human", "b-box", "it is me"); err != nil {
		t.Errorf("--human send: %v", err)
	}
}

// TestE2EAliasHandover walks the takeover path a person actually types.
func TestE2EAliasHandover(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "old", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "old", "shared")
	mustRun(t, "attach", "--session", "new", "--tool", "claude", "--root", dir)

	// The holder is still alive, so neither naming nor claiming may take it.
	if _, err := capture(t, "", "alias", "--session", "new", "shared"); err == nil {
		t.Error("alias took over a live session's inbox")
	}
	if _, err := capture(t, "", "claim", "--session", "new", "shared"); err == nil {
		t.Error("claim took over a live session's inbox")
	}
}

func TestE2EListAndPrune(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "b", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "b", "b-box")
	mustRun(t, "send", "--human", "b-box", "hello there")

	listed := mustRun(t, "ls", "--session", "b")
	for _, want := range []string{"b-box", "waiting", "hello there"} {
		if !strings.Contains(listed, want) {
			t.Errorf("ls output is missing %q:\n%s", want, listed)
		}
	}
	if out := mustRun(t, "ls", "--session", "b", "--expired"); !strings.Contains(out, "nothing here") {
		t.Errorf("a fresh message showed up as expired:\n%s", out)
	}
	if out := mustRun(t, "prune", "--dry-run"); !strings.Contains(out, "0 message(s) would be deleted") {
		t.Errorf("dry-run prune: %s", out)
	}
}

// TestE2EWorksWithoutPMRoadmap: pager must not need .agents to exist.
func TestE2EWorksWithoutPMRoadmap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "solo", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "solo", "solo-box")
	mustRun(t, "send", "--human", "solo-box", "no project management here")

	payload := `{"session_id":"solo","cwd":"` + dir + `","hook_event_name":"UserPromptSubmit","prompt":"go"}`
	out, err := capture(t, payload, "hook", "UserPromptSubmit")
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if !strings.Contains(out, "no project management here") {
		t.Errorf("delivery failed without a task store:\n%s", out)
	}
}

func TestE2EUnknownAndAmbiguousTargets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PAGER_DB", filepath.Join(dir, "msg.db"))

	mustRun(t, "attach", "--session", "b", "--tool", "claude", "--root", dir)
	mustRun(t, "alias", "--session", "b", "review-one")
	mustRun(t, "alias", "--session", "b", "review-two")

	if _, err := capture(t, "", "send", "--human", "nobody", "hi"); err == nil {
		t.Error("a send to an unknown target succeeded")
	}
	_, err := capture(t, "", "send", "--human", "review", "hi")
	if err == nil {
		t.Fatal("an ambiguous target succeeded, want the candidates reported")
	}
	if !strings.Contains(err.Error(), "review-one") || !strings.Contains(err.Error(), "review-two") {
		t.Errorf("the error does not list the candidates: %v", err)
	}
}
