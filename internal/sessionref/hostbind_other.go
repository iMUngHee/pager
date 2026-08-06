//go:build !darwin && !linux

package sessionref

// procInfo is unsupported outside darwin and linux. Detect then fails, which
// costs those platforms the third resolution tier — --session and PAGER_SESSION
// still work, and a send with neither is refused rather than misattributed.
func procInfo(_ int) (ppid int, start int64, cmd string, ok bool) {
	return 0, 0, "", false
}
