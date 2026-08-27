//go:build darwin

package sessionref

import (
	"bytes"

	"golang.org/x/sys/unix"
)

// procInfoSupported says this platform implements procInfo, so a failed lookup
// means the process is gone rather than that nobody can tell. See Alive.
const procInfoSupported = true

// procInfo reads the parent pid, start token, and an identifying command string
// for pid via sysctl.
//
// The command string is comm plus the executable path, never the full argv —
// see commandMatchesClient.
func procInfo(pid int) (ppid int, start int64, cmd string, ok bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return 0, 0, "", false
	}
	ppid = int(kp.Eproc.Ppid)
	start = int64(kp.Proc.P_starttime.Sec)*1000 + int64(kp.Proc.P_starttime.Usec)/1000
	return ppid, start, nullTerminated(kp.Proc.P_comm[:]) + " " + execPath(pid), true
}

// execPath returns the executable path from KERN_PROCARGS2, or "" when it is
// unavailable (permission, most often). comm alone then carries the match.
func execPath(pid int) string {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 4 {
		return ""
	}
	rest := raw[4:] // skip the argc prefix
	if i := bytes.IndexByte(rest, 0); i >= 0 {
		return string(rest[:i])
	}
	return string(rest)
}

func nullTerminated(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
