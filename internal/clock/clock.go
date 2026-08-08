// Package clock provides the emulator's controllable time source. Every
// timestamp the emulator stamps — operation completeAt, token exp checks,
// meta.created — flows through Store.Now, which delegates here, so advancing
// or freezing this clock makes LRO completion and token expiry testable
// without real sleeps. Mirrors entra-emulator's clock so the pair share
// testing idioms.
package clock

import (
	"sync"
	"time"
)

// Clock is a concurrency-safe, offsettable, freezable wall clock.
type Clock struct {
	mu       sync.RWMutex
	offset   int64 // seconds added to real time when not frozen
	frozen   bool
	frozenAt int64 // absolute epoch returned while frozen
	realNow  func() int64
	changed  chan struct{} // closed on mutation; see Changed
}

// New returns a clock tracking real time.
func New() *Clock {
	return &Clock{realNow: func() int64 { return time.Now().Unix() }}
}

// Now returns the current controlled time (epoch seconds).
func (c *Clock) Now() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.frozen {
		return c.frozenAt
	}
	return c.realNow() + c.offset
}

// SetOffset sets an absolute offset from real time and unfreezes.
func (c *Clock) SetOffset(seconds int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset = seconds
	c.frozen = false
	c.notifyChange()
}

// Advance moves the controlled time forward (or back) by delta seconds,
// honoring the frozen state.
func (c *Clock) Advance(seconds int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frozen {
		c.frozenAt += seconds
		c.notifyChange()
		return
	}
	c.offset += seconds
	c.notifyChange()
}

// Freeze pins time at the current controlled value.
func (c *Clock) Freeze() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frozen {
		return
	}
	c.frozenAt = c.realNow() + c.offset
	c.frozen = true
	c.notifyChange()
}

// Unfreeze resumes real-time tracking, preserving the frozen point as an
// offset so time is continuous.
func (c *Clock) Unfreeze() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.frozen {
		return
	}
	c.offset = c.frozenAt - c.realNow()
	c.frozen = false
	c.notifyChange()
}

// State reports the current offset and frozen status for the control API.
func (c *Clock) State() (offset int64, frozen bool, now int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.frozen {
		return c.frozenAt - c.realNow(), true, c.frozenAt
	}
	return c.offset, false, c.realNow() + c.offset
}

// Changed returns a channel that closes on the next mutation (Advance,
// SetOffset, Freeze, Unfreeze). A parked goroutine whose deadline lives on
// THIS clock cannot learn about an Advance any other way: virtual time moves
// without any real time passing, so a real-time timer alone would sleep
// through it. Callers re-arm by calling Changed again after each wake.
func (c *Clock) Changed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.changed == nil {
		c.changed = make(chan struct{})
	}
	return c.changed
}

// notifyChange closes the current change channel, waking every waiter.
// Callers hold c.mu.
func (c *Clock) notifyChange() {
	if c.changed != nil {
		close(c.changed)
		c.changed = nil
	}
}
