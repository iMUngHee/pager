//go:build linux

package sessionref

import (
	"os"
	"strconv"
	"strings"
)

// procInfoSupported says this platform implements procInfo, so a failed lookup
// means the process is gone rather than that nobody can tell. See Alive.
const procInfoSupported = true

// procInfo reads the parent pid, start token (starttime in clock ticks, which
// every reader on the machine sees identically), and comm plus argv0 from
// /proc/<pid>.
//
// argv0 rather than the full argv — see commandMatchesClient.
func procInfo(pid int) (ppid int, start int64, cmd string, ok bool) {
	statBytes, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, "", false
	}
	stat := string(statBytes)

	// comm (field 2) is parenthesised and may itself contain spaces or
	// parens, so split the remainder after the final ')'.
	rparen := strings.LastIndexByte(stat, ')')
	if rparen < 0 {
		return 0, 0, "", false
	}
	comm := ""
	if lparen := strings.IndexByte(stat, '('); lparen >= 0 && rparen > lparen {
		comm = stat[lparen+1 : rparen]
	}
	// After comm: fields[0] = state (field 3), fields[1] = ppid (field 4),
	// fields[19] = starttime (field 22).
	fields := strings.Fields(stat[rparen+1:])
	if len(fields) < 20 {
		return 0, 0, "", false
	}
	ppid, _ = strconv.Atoi(fields[1])
	start, _ = strconv.ParseInt(fields[19], 10, 64)

	cmdline, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	argv0 := string(cmdline)
	if i := strings.IndexByte(argv0, 0); i >= 0 {
		argv0 = argv0[:i]
	}
	return ppid, start, comm + " " + argv0, true
}
