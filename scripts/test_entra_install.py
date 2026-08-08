#!/usr/bin/env python3
"""The retry in e2e/entra_install.py, tested in both directions.

A retry is the easiest thing in a build to get silently wrong: too narrow and it
does not fire on the failure it was written for; too broad and it turns a real
breakage into three sleeps and the same error, with the diagnosis buried under
"attempt 3/3". Both look identical in a green log, so both are asserted.
"""

import importlib.util
import pathlib
import sys

spec = importlib.util.spec_from_file_location(
    "entra_install",
    pathlib.Path(__file__).resolve().parents[1] / "e2e" / "entra_install.py")
ei = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ei)

# The real message, from the run that prompted this helper (2026-08-08 04:39Z).
SUMDB_LAG = (
    "go: github.com/calvinchengx/entra-emulator/cmd/entra-emulator@v0.3.0: "
    "loading deprecation for github.com/calvinchengx/entra-emulator: "
    "github.com/calvinchengx/entra-emulator@v0.3.1: verifying go.mod: "
    "reading https://sum.golang.org/lookup/github.com/calvinchengx/entra-emulator@v0.3.1: "
    "404 Not Found\n\tserver response: not found: "
    "github.com/calvinchengx/entra-emulator@v0.3.1: invalid version: unknown revision v0.3.1"
)
# The same shape, but naming the PINNED version — a genuinely broken pin.
BROKEN_PIN = (
    "go: github.com/calvinchengx/entra-emulator/cmd/entra-emulator@v0.3.0: "
    "reading https://sum.golang.org/lookup/github.com/calvinchengx/entra-emulator@v0.3.0: "
    "404 Not Found\n\tserver response: not found: "
    "github.com/calvinchengx/entra-emulator@v0.3.0: invalid version: unknown revision v0.3.0"
)
COMPILE_ERROR = "cmd/entra-emulator/main.go:12:2: undefined: doesNotExist"


def fail(msg):
    print(f"FAIL: {msg}")
    sys.exit(1)


def check(name, cond):
    if not cond:
        fail(name)


def fake_run(outputs):
    """A subprocess.run stand-in that yields the given outcomes in order."""
    calls = []

    class Result:
        def __init__(self, rc, err):
            self.returncode, self.stdout, self.stderr = rc, "", err

    def run(cmd, **kw):
        calls.append(cmd)
        rc, err = outputs[min(len(calls) - 1, len(outputs) - 1)]
        return Result(rc, err)

    return run, calls


def with_stubs(outputs, attempts=3):
    """Run go_install against scripted outcomes, with PATH lookup and sleeping
    stubbed out so nothing touches the network or the clock."""
    real_run, real_which = ei.subprocess.run, ei.shutil.which
    run, calls = fake_run(outputs)
    slept = []
    ei.subprocess.run = run
    ei.shutil.which = lambda _: None
    try:
        result, error = None, None
        try:
            result = ei.go_install("entra-emulator", "example.com/mod/cmd/entra-emulator",
                                   "/tmp/work", version="v0.3.0", log=lambda *_: None,
                                   attempts=attempts, delay=0, sleep=slept.append)
        except RuntimeError as e:
            error = str(e)
        return result, error, calls, slept
    finally:
        ei.subprocess.run, ei.shutil.which = real_run, real_which


def main():
    # 1. The window: fails once with the sumdb lag, then succeeds. Must retry.
    result, error, calls, slept = with_stubs([(1, SUMDB_LAG), (0, "")])
    check("the sumdb window was not retried", len(calls) == 2)
    check("a successful retry still raised", error is None and result)
    check("the retry did not wait between attempts", slept == [0])

    # 2. A broken PIN names the version being installed. Must NOT be retried —
    #    three sleeps and the same error is how a retry hides a real breakage.
    result, error, calls, slept = with_stubs([(1, BROKEN_PIN)])
    check("a broken pin was retried", len(calls) == 1)
    check("a broken pin did not raise", error is not None)
    check("the broken-pin error lost go's own message", "unknown revision v0.3.0" in error)

    # 3. A compile error is not a proxy problem. Fail fast.
    _, error, calls, _ = with_stubs([(1, COMPILE_ERROR)])
    check("a compile error was retried", len(calls) == 1)
    check("the compile error was swallowed", error and "undefined: doesNotExist" in error)

    # 4. Exhausted retries surface go's output, not just "failed after 3".
    _, error, calls, _ = with_stubs([(1, SUMDB_LAG)], attempts=3)
    check("attempts were not exhausted", len(calls) == 3)
    check("the final error lost the diagnosis", error and "sum.golang.org" in error)

    # 5. Already on PATH: no install at all.
    real_which = ei.shutil.which
    ei.shutil.which = lambda _: "/usr/local/bin/entra-emulator"
    try:
        got = ei.go_install("entra-emulator", "example.com/mod/cmd/entra-emulator", "/tmp/w",
                            version="v0.3.0", log=lambda *_: None)
        check("a binary on PATH was reinstalled", got == "/usr/local/bin/entra-emulator")
    finally:
        ei.shutil.which = real_which

    # 6. The version is DERIVED from go.mod, which is the other half of this
    #    change — nine copies of a pin maintained by comment are now one read.
    version = ei.module_version(ei.ENTRA_MODULE)
    check("the entra-emulator version did not come from go.mod",
          version.startswith("v") and version.count(".") >= 2)
    try:
        ei.module_version("example.com/not/pinned")
        fail("an unpinned module did not raise — it would install some other version")
    except RuntimeError:
        pass

    print(f"entra install helper: PASS (retry scoped; go.mod pins {version})")


if __name__ == "__main__":
    main()
