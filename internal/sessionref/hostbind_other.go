//go:build !darwin && !linux && !windows

package sessionref

// procInfoSupported says this platform cannot implement procInfo, so Alive
// answers "cannot tell" rather than reporting every process as gone.
const procInfoSupported = false

// procInfo is unsupported outside darwin, linux and windows. Detect then fails,
// which costs those platforms the third resolution tier — --session and
// PAGER_SESSION still work, and a send with neither is refused rather than
// misattributed.
func procInfo(_ int) (ppid int, start int64, cmd string, ok bool) {
	return 0, 0, "", false
}

// procSource answers process queries for the life of one operation. There is
// nothing to answer them from here.
type procSource struct{}

func newProcSource() procSource { return procSource{} }

func (procSource) info(pid int) (ppid int, start int64, cmd string, ok bool) {
	return procInfo(pid)
}

// startToken cannot tell, which is not the same as reporting the process gone:
// every recorded host would otherwise read as dead on a platform that simply
// has no way to look.
func (procSource) startToken(_ int) (start int64, exists, known bool) {
	return 0, false, false
}
