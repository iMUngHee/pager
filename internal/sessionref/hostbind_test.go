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
	resultPrefix   = "HOST_RESULT"
)

func TestDetectHostThroughShell(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("procInfo is unsupported on %s", runtime.GOOS)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	// The host process must be *named* claude — that name is the whole basis
	// of the match.
	hostPath := filepath.Join(t.TempDir(), Claude)
	copyExecutable(t, self, hostPath)

	cmd := exec.Command(hostPath, "-test.run=^TestHostHelper$")
	cmd.Env = append(os.Environ(), envHostHelper+"=1", envSelfPath+"="+self)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("host helper failed: %v\n%s", err, out)
	}

	ok, client, pid, start := parseResult(t, string(out))
	if !ok {
		t.Fatalf("Detect reported failure from under a claude-named host\n%s", out)
	}
	if client != Claude {
		t.Errorf("client = %q, want %q", client, Claude)
	}
	if pid != cmd.Process.Pid {
		t.Errorf("detected pid = %d, want the host process %d", pid, cmd.Process.Pid)
	}
	if start == 0 {
		t.Error("start token is 0; pid reuse would not be detectable")
	}
}

// TestHostHelper runs as the process named "claude". It puts a shell between
// itself and the probe so the walk has to climb more than one level.
func TestHostHelper(t *testing.T) {
	if os.Getenv(envHostHelper) == "" {
		t.Skip("runs only when spawned by TestDetectHostThroughShell")
	}
	self := os.Getenv(envSelfPath)
	cmd := exec.Command("/bin/sh", "-c", "'"+self+"' -test.run='^TestProbeHelper$'")
	cmd.Env = append(os.Environ(),
		envHostHelper+"=", // stop the recursion
		envProbeHelper+"=1",
		"PAGER_CLIENT="+Claude, // pin the label so the walk is deterministic
		"PAGER_SESSION=",       // tier 2 must not leak in from the outer env
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
