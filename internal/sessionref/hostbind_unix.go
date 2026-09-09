//go:build darwin || linux

package sessionref

// procSource answers process queries for the life of one operation.
//
// On darwin and linux every answer is a targeted read about a single pid — a
// sysctl, or a file under /proc — so there is nothing to amortise across a walk
// and the source holds no state. Windows is where the seam earns its keep.
type procSource struct{}

func newProcSource() procSource { return procSource{} }

func (procSource) info(pid int) (ppid int, start int64, cmd string, ok bool) {
	return procInfo(pid)
}

// startToken reports the start token for pid, whether the process exists, and
// whether this platform could tell.
//
// Here the read that fails is the same read that proves existence, so a failure
// is a definite "no such process" rather than an absence of information — known
// is therefore always true.
func (procSource) startToken(pid int) (start int64, exists, known bool) {
	_, start, _, ok := procInfo(pid)
	return start, ok, true
}
