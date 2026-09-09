package sessionref

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The ancestry walk is the load-bearing part of the session-binding contract
// and cannot be proven with a unit test: it reads the real process tree. These
// three tests build one.
//
// TestDetectHostThroughShell copies the test binary to a file named "claude"
// and runs it; that process starts a shell; the shell runs the original binary,
// which calls Detect. The shape mirrors production exactly — an agent's Bash
// tool runs `pager` through a shell, and the host two levels up is what has to
// be found. The two helpers only act when the parent set their env marker, so a
// plain `go test` run skips them.

const (
	envHostHelper  = "PAGER_TEST_HOST_HELPER"
	envProbeHelper = "PAGER_TEST_PROBE_HELPER"
	envSelfPath    = "PAGER_TEST_SELF"
	envPinClient   = "PAGER_TEST_PIN_CLIENT"
	resultPrefix   = "HOST_RESULT"
)

// runHostHarness builds the three-level ancestry — a process named "claude", a
// shell, then a probe that calls Detect — and returns what the probe reported
// plus the pid of the injected host. pin is the PAGER_CLIENT the probe runs
// under.
func runHostHarness(t *testing.T, pin string) (ok bool, client string, pid int, start int64, hostPid int) {
	t.Helper()
	if !procInfoSupported {
		t.Skipf("procInfo is unsupported on %s", runtime.GOOS)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	// The host process must be *named* claude — that name is the whole basis
	// of the match. Windows will not execute a file without an executable
	// extension, and commandMatchesClient trims .exe precisely so the name
	// still matches with it.
	hostPath := filepath.Join(t.TempDir(), Claude+exeSuffix())
	copyExecutable(t, self, hostPath)

	cmd := exec.Command(hostPath, "-test.run=^TestHostHelper$")
	cmd.Env = append(os.Environ(),
		envHostHelper+"=1", envSelfPath+"="+self, envPinClient+"="+pin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("host helper failed: %v\n%s", err, out)
	}

	ok, client, pid, start = parseResult(t, string(out))
	return ok, client, pid, start, cmd.Process.Pid
}

func TestDetectHostThroughShell(t *testing.T) {
	ok, client, pid, start, hostPid := runHostHarness(t, Claude)
	if !ok {
		t.Fatal("Detect reported failure from under a claude-named host")
	}
	if client != Claude {
		t.Errorf("client = %q, want %q", client, Claude)
	}
	if pid != hostPid {
		t.Errorf("detected pid = %d, want the host process %d", pid, hostPid)
	}
	// A detected host must carry an identity, not just a pid: Detect refuses a
	// match whose token it could not read, precisely so a recycled pid cannot
	// resolve to the session that held it before.
	if !(Instance{Pid: pid, Start: start}).Valid() {
		t.Errorf("Detect reported a host with no usable identity (pid=%d start=%d); "+
			"pid reuse would not be detectable", pid, start)
	}
}

// TestDetectPinRejectsNonCandidateHost exercises the walk in the failing
// direction: a host named "claude" sits directly above the probe while
// PAGER_CLIENT pins codex, so the one host the walk can actually see is not a
// candidate and must not be accepted.
//
// The assertion is an invariant rather than a count, because the harness cannot
// own the whole ancestry — the walk climbs up to maxWalkDepth levels and passes
// straight through the injected host into whatever launched `go test`. Its
// predecessor asserted that no codex is found at all, which is a fact about the
// machine and not about the code: it held on CI and was false inside a real
// Codex session. What is always true is that the pinned-out claude was rejected,
// so that is what this checks — either nothing resolved, or something resolved
// that is a codex somewhere above and is not the injected host.
func TestDetectPinRejectsNonCandidateHost(t *testing.T) {
	ok, client, pid, _, hostPid := runHostHarness(t, Codex)
	if !ok {
		if client != Unknown {
			t.Errorf("client = %q, want %q when nothing resolved", client, Unknown)
		}
		return
	}
	if client != Codex {
		t.Errorf("client = %q, want %q — only the pinned candidate may resolve", client, Codex)
	}
	if pid == hostPid {
		t.Errorf("accepted the claude-named host %d while pinned to %q", hostPid, Codex)
	}
}

// TestHostHelper runs as the process named "claude". It puts a shell between
// itself and the probe so the walk has to climb more than one level.
func TestHostHelper(t *testing.T) {
	if os.Getenv(envHostHelper) == "" {
		t.Skip("runs only when spawned by TestDetectHostThroughShell")
	}
	self := os.Getenv(envSelfPath)
	pin := os.Getenv(envPinClient)
	if pin == "" {
		pin = Claude
	}
	cmd := shellRunning(self, "-test.run=^TestProbeHelper$")
	cmd.Env = append(os.Environ(),
		envHostHelper+"=", // stop the recursion
		envProbeHelper+"=1",
		"PAGER_CLIENT="+pin, // pin the label so the walk is deterministic
		"PAGER_SESSION=",    // tier 2 must not leak in from the outer env
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		t.Fatalf("relay probe output: %v", err)
	}
}

// TestProbeHelper is the grandchild standing in for `pager send`: it knows
// nothing about its session and must find the host by walking up.
func TestProbeHelper(t *testing.T) {
	if os.Getenv(envProbeHelper) == "" {
		t.Skip("runs only when spawned by TestHostHelper")
	}
	client, host, ok := Detect()
	fmt.Printf("%s %t %s %d %d\n", resultPrefix, ok, client, host.Pid, host.Start)
}

func parseResult(t *testing.T, out string) (ok bool, client string, pid int, start int64) {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 || fields[0] != resultPrefix {
			continue
		}
		ok, _ = strconv.ParseBool(fields[1])
		client = fields[2]
		pid, _ = strconv.Atoi(fields[3])
		start, _ = strconv.ParseInt(fields[4], 10, 64)
		return ok, client, pid, start
	}
	t.Fatalf("no %s line in helper output:\n%s", resultPrefix, out)
	return
}

// exeSuffix is the extension an executable must carry to be runnable at all.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// shellRunning returns a command that runs prog through the platform shell.
//
// The shell is the point: it is the extra ancestry level the walk has to climb
// past to reach the host. Arguments are passed as separate argv entries on
// Windows rather than as one string, which keeps cmd.exe away from the regex —
// ^ is its escape character, and -test.run patterns are full of them.
func shellRunning(prog string, args ...string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", append([]string{"/c", prog}, args...)...)
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + a + "'"
	}
	return exec.Command("/bin/sh", "-c", "'"+prog+"' "+strings.Join(quoted, " "))
}

// shellScript runs a one-line script through the platform shell. Used only for
// a throwaway process whose exit is the whole point.
func shellScript(script string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", script)
	}
	return exec.Command("/bin/sh", "-c", script)
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatalf("copy to %s: %v", dst, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", dst, err)
	}
}

// TestAliveSeparatesGoneFromUnknowable is the contract Alive exists for: three
// answers, not two.
//
// The recycled-pid case is the reason the start token is compared at all, and it
// is checked here by asking about this very process with a token that is one off
// — a pid that is unquestionably in use, whose instance is still not the one
// recorded. A liveness check built on `kill -0` cannot tell those apart, which is
// what a consumer doing its own process check gets wrong.
func TestAliveSeparatesGoneFromUnknowable(t *testing.T) {
	if !procInfoSupported {
		t.Skip("this platform cannot read process info, so every answer is unknown")
	}
	// Through the same seam Alive uses, so the token compared here is the one
	// it would read.
	start, exists, known := newProcSource().startToken(os.Getpid())
	if !exists || !known || start == 0 {
		t.Fatalf("startToken on the test process itself = (%d, %t, %t), want a token", start, exists, known)
	}

	// A process that has certainly exited. Its pid may be recycled later, but
	// then its start token differs and the answer is still "not the one we
	// recorded" — the same expectation either way.
	dead := shellScript("exit 0")
	if err := dead.Run(); err != nil {
		t.Fatalf("run throwaway process: %v", err)
	}

	for _, tc := range []struct {
		name         string
		inst         Instance
		alive, known bool
	}{
		{"this process", Instance{Pid: os.Getpid(), Start: start}, true, true},
		{"a recycled pid", Instance{Pid: os.Getpid(), Start: start + 1}, false, true},
		{"a process that exited", Instance{Pid: dead.Process.Pid, Start: start}, false, true},
		{"no host was ever detected", Instance{}, false, false},
		{"pid 1 is not a host", Instance{Pid: 1, Start: start}, false, false},
		// A pid with no token cannot be told from the next process to hold
		// that number, so it is unknowable rather than alive. Windows can
		// produce it: the creation time needs a handle this user may be
		// refused. Answering "alive" here is what would let a recycled pid
		// pass for the original.
		{"a pid whose token was unreadable", Instance{Pid: os.Getpid()}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alive, known := Alive(tc.inst)
			if alive != tc.alive || known != tc.known {
				t.Errorf("Alive(%+v) = (%t, %t), want (%t, %t)",
					tc.inst, alive, known, tc.alive, tc.known)
			}
		})
	}
}

// TestAliveAtMatchesAlive keeps the adapter honest. It exists only to spare its
// callers rebuilding an Instance, so the one thing it must never do is answer
// differently from what it wraps.
func TestAliveAtMatchesAlive(t *testing.T) {
	if !procInfoSupported {
		t.Skip("this platform cannot read process info, so every answer is unknown")
	}
	// Through the same seam Alive uses, so the token compared here is the one
	// it would read.
	start, exists, known := newProcSource().startToken(os.Getpid())
	if !exists || !known || start == 0 {
		t.Fatalf("startToken on the test process itself = (%d, %t, %t), want a token", start, exists, known)
	}

	for _, inst := range []Instance{
		{Pid: os.Getpid(), Start: start},
		{Pid: os.Getpid(), Start: start + 1},
		{},
		{Pid: 1, Start: start},
		{Pid: os.Getpid()},
	} {
		wantAlive, wantKnown := Alive(inst)
		alive, known := AliveAt(inst.Pid, inst.Start)
		if alive != wantAlive || known != wantKnown {
			t.Errorf("AliveAt(%d, %d) = (%t, %t), want Alive's (%t, %t)",
				inst.Pid, inst.Start, alive, known, wantAlive, wantKnown)
		}
	}
}
