package wake

import (
	"net"

	"golang.org/x/sys/windows"
)

// pipeServerPID asks the kernel which process created the server end of an
// already connected named pipe. It is the pipe counterpart of peerPID: the
// pipe name is first-come in the LOCAL\ namespace, so anything owned by this
// user could have created it before the session did, but the server pid is
// recorded by the kernel and cannot be forged by whoever is listening.
func pipeServerPID(c net.Conn) (int, bool) {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return 0, false
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(f.Fd()), &pid); err != nil {
		return 0, false
	}
	return int(pid), true
}
