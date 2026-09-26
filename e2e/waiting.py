#!/usr/bin/env python3
"""Bounded waiting for the e2e harnesses -- the Python analogue of internal/testsupport.

WHY THIS EXISTS. `internal/testsupport/wait.go` gives the Go suite two helpers,
`WaitFor` and `StaysFalse`, and docs/60-test-flakiness.md records what they were
for: three tests that slept a fixed interval and then read a verdict, all three
asserting a NEGATIVE. The Python surface had the same shape and no such helpers,
so every harness open-coded its waiting and one of them open-coded it wrongly.

THE NEGATIVE IS THE DANGEROUS CASE, and it is worth stating plainly because it
is the reason `stays_empty` is not just a tidier sleep. Sleeping two seconds and
then finding that no pipeline run started is consistent with two different
worlds:

    * there is no trigger, which is what the test claims; or
    * there is a trigger that has not been scheduled yet.

On a laptop it is the first. On a loaded CI runner it can be the second, and
then the assertion passes for a reason unrelated to the code under test. The
test does not fail -- it stops meaning anything, silently, on exactly the
machine where it matters most. Checking the negative across the WHOLE window
instead of at the single instant one sleep happened to land on is what turns
that back into an assertion: a trigger that fires 10ms in now fails, where
before it would have been missed entirely.

ON THE WINDOW. `stays_empty` does not engineer away the guess about how long is
"long enough for the wrong thing to have happened" -- nothing can. It makes the
guess VISIBLE, at the call site, in a parameter named after what it bounds,
rather than leaving it as a bare `time.sleep(2)` with a comment explaining that
it is not really a sleep. That is the same trade `StaysFalse` makes and the
reason its window is a required argument there too.

WHERE THIS LIVES. `e2e/` is already a shared import root: `e2e/adls-sdk/run.py`,
`e2e/fabric-cicd/run.py` and seven others do

    sys.path.insert(0, os.path.join(REPO, "e2e"))

to reach `e2e/entra_install.py`. This module follows that convention rather than
inventing a layout, so a harness importing it needs no new wiring.

Stdlib only, deliberately: these run inside e2e harnesses whose dependency
groups vary per suite, and a helper that cannot be imported everywhere would be
open-coded again in the suites that could not reach it.
"""
import time

__all__ = ["stays_empty", "wait_for"]

# Poll interval. Small enough that a fast success returns promptly and a
# transient failure inside `stays_empty` is actually observed, large enough not
# to hammer a control plane over a multi-second window.
_INTERVAL = 0.05


def wait_for(timeout, cond, msg):
    """Poll `cond` until it is truthy; raise at the deadline.

    Returns whatever `cond` last returned, so a caller can wait for a value and
    use it in one step:

        runs = wait_for(30.0, pipeline_runs, "no run ever started")

    The deadline bounds the FAILURE case only -- a success returns as soon as
    `cond` is true, so a generous timeout costs nothing on a healthy run and is
    what keeps a loaded runner from failing for being slow. That reasoning is
    `awaitJob`'s in internal/api/notebookdrive_test.go, where a 5s ceiling had
    produced exactly the runner-starvation flake a 30s one removes.
    """
    deadline = time.monotonic() + timeout
    got = None
    while True:
        got = cond()
        if got:
            return got
        # Checked AFTER the probe and BEFORE the sleep, so a condition that is
        # already true on entry never sleeps, and the last probe before the
        # deadline is not thrown away unread.
        if time.monotonic() >= deadline:
            raise AssertionError(f"{msg} (waited {timeout:g}s)")
        time.sleep(_INTERVAL)


def stays_empty(window, probe, msg):
    """Assert `probe()` stays empty/falsy for the WHOLE of `window` seconds.

    The negative assertion, checked continuously rather than once. `probe` is
    called repeatedly until the window elapses, and the first non-empty answer
    fails immediately -- naming what was found, because "a trigger fired" and
    "the wrong trigger fired" are different bugs and the value distinguishes
    them.

    `window` is positional-or-keyword and callers are expected to pass it by
    name, so the guess it encodes is legible where the claim is made.
    """
    deadline = time.monotonic() + window
    while time.monotonic() < deadline:
        got = probe()
        if got:
            raise AssertionError(f"{msg}: {got!r}")
        time.sleep(_INTERVAL)
    # Probed once more after the window closes: without this the last _INTERVAL
    # of the window is never actually observed, so the assertion would cover
    # slightly less time than it claims to.
    got = probe()
    if got:
        raise AssertionError(f"{msg}: {got!r}")
