//go:build darwin

package wake

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerPID asks the kernel which process is on the other end of an already
// connected unix socket.
//
// The answer cannot be forged by whoever is listening, which is the whole
// reason to ask: the socket path is a filesystem name anything owned by this
// user could have created.
func peerPID(c *net.UnixConn) (int, bool) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, false
	}
	var (
		pid     int
		lookErr error
	)
	if err := raw.Control(func(fd uintptr) {
		pid, lookErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil || lookErr != nil {
		return 0, false
	}
	return pid, true
}
