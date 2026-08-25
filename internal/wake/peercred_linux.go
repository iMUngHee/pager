//go:build linux

package wake

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerPID asks the kernel which process is on the other end of an already
// connected unix socket. See peercred_darwin.go for why it is worth asking.
func peerPID(c *net.UnixConn) (int, bool) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, false
	}
	var (
		cred    *unix.Ucred
		lookErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, lookErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || lookErr != nil || cred == nil {
		return 0, false
	}
	return int(cred.Pid), true
}
