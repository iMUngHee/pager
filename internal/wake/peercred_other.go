//go:build !darwin && !linux

package wake

import "net"

// peerPID has no portable answer outside darwin and linux.
//
// Reporting "unknown" rather than a guess keeps the caller honest: it skips the
// endpoint check instead of comparing against a fabricated pid. The poke still
// carries a token the listener must recognise, so an unrelated process that
// squatted the path gets nothing useful — the endpoint check is a second lock,
// not the only one.
func peerPID(*net.UnixConn) (int, bool) { return 0, false }
