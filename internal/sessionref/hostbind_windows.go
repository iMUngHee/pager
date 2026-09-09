//go:build windows

package sessionref

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// procInfoSupported says this platform implements procInfo, so a failed lookup
// means the process is gone rather than that nobody can tell. See Alive.
const procInfoSupported = true

// procInfo reads the parent pid, start token (process creation time in
// nanoseconds, fixed for the life of the process and seen identically by every
// reader on the machine), and the image name plus image path for pid.
//
// It takes two calls because no single Windows API carries all of it, and the
// two answer different questions:
//
//   - The toolhelp snapshot is the only documented source of a parent pid, and
//     it doubles as the existence oracle. A pid absent from it is a process that
//     is gone, which is the definite "no" Alive is built to distinguish from
//     "cannot tell".
//   - Creation time and the full image path need an open handle, and a handle
//     can be refused for a process this user does not own. Those therefore
//     degrade to 0 and "" instead of failing the lookup — the same way darwin's
//     execPath degrades when procargs2 is unreadable. Existence is still known;
//     only the extra identifying detail is missing.
//
// Image name plus path, never the full command line — see commandMatchesClient.
func procInfo(pid int) (ppid int, start int64, cmd string, ok bool) {
	if pid <= 0 {
		return 0, 0, "", false
	}

	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, 0, "", false
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	found := false
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if entry.ProcessID == uint32(pid) {
			found = true
			break
		}
	}
	if !found {
		return 0, 0, "", false
	}
	ppid = int(entry.ParentProcessID)
	cmd = windows.UTF16ToString(entry.ExeFile[:])

	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ppid, 0, cmd, true
	}
	defer windows.CloseHandle(handle)

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err == nil {
		start = creation.Nanoseconds()
	}
	if path := imagePath(handle); path != "" {
		cmd += " " + path
	}
	return ppid, start, cmd, true
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
