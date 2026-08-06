// Package clock provides an injectable time source.
//
// Every deadline in pager is derived from Clock.Now: lease expiry, heartbeat
// staleness, the automatic-injection window, and retention. Tests inject Fake
// and advance it, so no test waits on real time.
package clock

import (
	"sync"
	"time"
)

// Clock is the time source.
type Clock interface {
	Now() time.Time
}

// System is the real clock, used everywhere outside tests.
type System struct{}

// Now returns the current wall-clock time.
func (System) Now() time.Time { return time.Now() }

// Fake is a manually advanced clock. Concurrency tests advance it from one
// goroutine while others read it, so every access is mutex-guarded.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake positioned at now.
func NewFake(now time.Time) *Fake { return &Fake{now: now} }

// Now returns the fake's current position.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the fake forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}
