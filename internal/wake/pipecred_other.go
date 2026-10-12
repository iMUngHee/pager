//go:build !windows

package wake

import "net"

// pipeServerPID has nothing to ask outside Windows: dialClaude only ever
// returns a unix socket here, and peerPID covers those.
func pipeServerPID(net.Conn) (int, bool) { return 0, false }
