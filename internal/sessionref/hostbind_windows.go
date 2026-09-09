//go:build windows

package sessionref

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procInfoSupported says this platform implements procInfo, so a failed lookup
// means the process is gone rather than that nobody can tell. See Alive.
const procInfoSupported = true

// procSource answers process queries for the life of one operation.
//
// Windows is why this seam exists. A parent pid is only available from a
// toolhelp snapshot of the entire process table, so answering per pid would
// rescan every process on the machine once per ancestry level — up to
// maxWalkDepth times for a single Detect, and that walk runs its full length
// whenever there is no host to find. The source takes the snapshot at most once
// and answers every info call from it.
//
// startToken needs no parent, so it never touches the snapshot at all. That
// matters because the roster and the inbox listing probe one recorded process
// per row: a snapshot each would make rendering a table cost the process count
// of the machine times the number of rows.
type procSource struct {
	table map[int]procEntry
	taken bool
}

// procEntry is what one snapshot row carries: the two facts only the snapshot
// has.
type procEntry struct {
	ppid int
	exe  string
}

func newProcSource() *procSource { return &procSource{} }

// info reads the parent pid, start token (process creation time in
// nanoseconds, fixed for the life of the process and seen identically by every
// reader on the machine), and the image name plus image path for pid.
//
// The snapshot doubles as the existence oracle: a pid absent from it is a
// process that is gone, which is the definite "no" Alive is built to
// distinguish from "cannot tell".
//
// Creation time and the full image path need an open handle, and a handle can
// be refused for a process this user does not own. Those therefore degrade to 0
// and "" instead of failing the lookup — the same way darwin's execPath
// degrades when procargs2 is unreadable. Existence is still known; only the
// identifying detail is missing, and Detect refuses a host it cannot identify
// rather than recording an ambiguous one.
//
// Image name plus path, never the full command line — see commandMatchesClient.
func (s *procSource) info(pid int) (ppid int, start int64, cmd string, ok bool) {
	if pid <= 0 {
		return 0, 0, "", false
	}
	if !s.taken {
		s.table = snapshotProcesses()
		s.taken = true
	}
	entry, found := s.table[pid]
	if !found {
		return 0, 0, "", false
	}
	cmd = entry.exe

	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return entry.ppid, 0, cmd, true
	}
	defer windows.CloseHandle(handle)

	if token, running := creationToken(handle); running {
		start = token
	}
	if path := imagePath(handle); path != "" {
		cmd += " " + path
	}
	return entry.ppid, start, cmd, true
}

// startToken answers Alive's question without a snapshot.
//
// Opening the process is the whole check: the handle carries the creation time,
// and the error says which of the three answers applies. Only a pid Windows
// rejects outright is a definite "gone" — a refused handle means the process is
// there and this user may not look, which is exactly "cannot tell".
//
// A process that has exited can still be openable while something holds a
// handle to it, so the exit time is checked too. The snapshot would have
// omitted it; this reports it gone for the same reason.
func (s *procSource) startToken(pid int) (start int64, exists, known bool) {
	if pid <= 0 {
		return 0, false, true
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return 0, false, true
		}
		return 0, false, false
	}
	defer windows.CloseHandle(handle)

	token, running := creationToken(handle)
	if !running {
		return 0, false, true
	}
	if token == 0 {
		// The process is there and its identity is unreadable. Reporting it
		// gone would strand its mail; reporting it alive would let a recycled
		// pid pass for the original.
		return 0, false, false
	}
	return token, true, true
}

// snapshotProcesses reads the whole process table once, keyed by pid. An empty
// map is a failed snapshot, which reads as every pid being absent — the same
// answer a caller gets for a process that is gone.
func snapshotProcesses() map[int]procEntry {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snapshot)

	table := make(map[int]procEntry)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		table[int(entry.ProcessID)] = procEntry{
			ppid: int(entry.ParentProcessID),
			exe:  windows.UTF16ToString(entry.ExeFile[:]),
		}
	}
	return table
}

// creationToken returns the start token behind an open handle, and whether the
// process is still running.
//
// The exit status comes from GetExitCodeProcess rather than from the exit time
// GetProcessTimes also reports: that field is documented as *undefined* for a
// process that has not exited, so reading it would let a live host be called
// definitely gone — the one answer this package must never invent, because it
// strands mail nobody abandoned. GetExitCodeProcess is the call that defines an
// answer for a running process.
//
// Its documented cost is that a process exiting with status 259 is
// indistinguishable from one still running. Microsoft's own guidance is not to
// use that value as an exit code, and the trade lands the right way round here:
// the failure is a dead host briefly reading as live, not a live host reading
// as dead.
//
// A running process whose token cannot be read is reported as running with a
// zero token, which startToken turns into "cannot tell".
func creationToken(handle windows.Handle) (start int64, running bool) {
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return 0, true
	}
	if code != uint32(windows.STATUS_PENDING) {
		return 0, false
	}

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return 0, true
	}
	return creation.Nanoseconds(), true
}

// imagePath returns the full executable path behind an open process handle, or
// "" when it is unavailable. The image name from the snapshot then carries the
// match on its own.
func imagePath(handle windows.Handle) string {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(handle, 0, &buf[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}
