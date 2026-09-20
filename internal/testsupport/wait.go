package testsupport

import (
	"testing"
	"time"
)

// The timing vocabulary this suite uses, in two functions.
//
// Almost every timing-coupled test in this repository already polls inside a
// loop guarded by a deadline, which is the correct shape: a fast machine
// returns immediately and a slow one is only bounded, never raced. Three did
// not. Each slept a fixed interval and then read a verdict, and each asserted a
// NEGATIVE — "no engine ran this job", "nothing was delivered after Close" —
// which is the one shape where a fixed sleep is not merely inelegant but
// load-dependent: the assertion passes because the thing under test had not
// finished yet, and on a loaded runner it would have.
//
// StaysFalse is the helper those three wanted. It makes the window explicit and
// polls across it, so the test fails the moment the condition becomes true
// rather than at whatever instant a single sleep happened to land. WaitFor is
// its positive twin, kept here so the two halves of the vocabulary live
// together — a test written against one can reach for the other without
// wondering whether it exists.
//
// Both take the message as a value rather than a format+args pair: the caller's
// own failure text is the thing a reader needs, and a helper that swallowed it
// into "condition was not met" would be a worse failure than the sleep it
// replaced.

// pollInterval is how often both helpers re-evaluate their condition.
//
// Small enough that a fast machine sees a change almost immediately, large
// enough that a condition doing real work (an HTTP round trip through an
// httptest server, a store read) is not called thousands of times per second.
// The same 10ms the hand-written polling loops in internal/api and
// internal/store already use.
const pollInterval = 10 * time.Millisecond

// WaitFor polls cond until it returns true, failing the test with msg if the
// timeout passes first.
//
// The timeout bounds only the FAILURE case — nothing here measures how long
// anything took, so a generous value costs a passing test nothing and removes a
// whole class of runner-starvation flakes.
func WaitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (waited %s)", msg, timeout)
		}
		time.Sleep(pollInterval)
	}
}

// StaysFalse asserts that cond never becomes true for the whole of window,
// failing with msg at the first moment it does.
//
// This is what a test asserting "nothing should happen" actually means, and it
// is NOT the same as sleeping for window and looking once. A single sleep reads
// the condition at one instant: if the thing under test fires early the test
// still passes (the state may have been overwritten, or the event consumed),
// and if the runner is loaded the sleep can expire before the thing under test
// has even been scheduled — so the pass is evidence of nothing. Polling across
// the window catches the event wherever in it the event lands, which is the
// assertion the caller wanted to make.
//
// The window still has to be chosen, and it is still a guess about how long is
// "long enough for the wrong thing to have happened". That guess is now
// VISIBLE, at the call site, in a parameter named after what it bounds — rather
// than buried in a bare time.Sleep whose comment has to explain that it is not
// really a sleep.
func StaysFalse(t *testing.T, window time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(window)
	for {
		if cond() {
			t.Fatal(msg)
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(pollInterval)
	}
}
