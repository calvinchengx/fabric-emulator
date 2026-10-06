#!/usr/bin/env python3
"""The retry in e2e/entra_install.py, tested in both directions — and the rule
that a PATH hit must be a binary this harness can terminate.

A retry is the easiest thing in a build to get silently wrong: too narrow and it
does not fire on the failure it was written for; too broad and it turns a real
breakage into three sleeps and the same error, with the diagnosis buried under
"attempt 3/3". Both look identical in a green log, so both are asserted.

The PATH rule has the same shape. A version-manager shim on PATH runs the
emulator as a CHILD, so `terminate()` reaches the shim, `wait()` returns -15, and
the teardown reports success while the emulator keeps its port until the machine
reboots. Nothing fails at the time — which is precisely why it is asserted here
rather than trusted to be noticed.
"""

import importlib.util
import os
import pathlib
import sys
import tempfile

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
# The same window, if a future Go release rewords it out of all recognition.
# It still names a version OTHER than the pin, which is the structural fact the
# classification rests on rather than the prose.
REWORDED = (
    "go: could not complete module graph for github.com/calvinchengx/entra-emulator: "
    "checksum lookup unavailable for github.com/calvinchengx/entra-emulator@v0.4.7 "
    "via https://sum.golang.org/"
)
# Module resolution failed and named no version at all — nothing for the
# structural test to work with, which is what a rewording that drops versions
# would look like.
UNCLASSIFIED = "go: module lookup disabled by GOFLAGS; see https://proxy.golang.org/ for details"


class RetryContractError(AssertionError):
    """Raised instead of exiting, so pytest can drive these assertions too."""


def fail(msg):
    raise RetryContractError(msg)


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


def _write(directory, name, content):
    """A file with the given first bytes — the only thing the PATH rule reads."""
    path = os.path.join(directory, name)
    with open(path, "wb") as fh:
        fh.write(content)
    os.chmod(path, 0o755)
    return path


def _install_with_path(what_which_returns, go_bin_dir=None):
    """go_install with PATH stubbed to return `what_which_returns`, and any
    install stubbed to succeed. Returns the path it chose."""
    real_run, real_which, real_bin = ei.subprocess.run, ei.shutil.which, ei._go_bin_dir
    run, _ = fake_run([(0, "")])
    ei.subprocess.run = run
    ei.shutil.which = lambda _: what_which_returns
    ei._go_bin_dir = lambda: go_bin_dir
    try:
        return ei.go_install("entra-emulator", "example.com/mod/cmd/entra-emulator", "/tmp/w",
                             version="v0.3.0", log=lambda *_: None)
    finally:
        ei.subprocess.run, ei.shutil.which, ei._go_bin_dir = real_run, real_which, real_bin


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

    # 3. The SAME window, reworded beyond recognition, is still retried — the
    #    classification is structural (it names a version other than the pin),
    #    not a match against Go's prose, which would be a second list
    #    maintained against someone else's changelog.
    result, error, calls, _ = with_stubs([(1, REWORDED), (0, "")])
    check("a reworded propagation failure was not retried", len(calls) == 2)
    check("the reworded retry did not then succeed", error is None and result)

    # 3b. Module resolution that names NO version cannot be classified either
    #     way. Not retried — and it says so, so a rewording that drops version
    #     strings is diagnosable instead of silently disabling the retry.
    _, error, calls, _ = with_stubs([(1, UNCLASSIFIED)])
    check("an unclassifiable module failure was retried", len(calls) == 1)
    check("an unclassifiable module failure was silent about it",
          error and "has stopped working" in error)

    # 3c. The note must NOT fire on failures that WERE classified, or it is
    #     noise on every broken pin.
    _, error, _, _ = with_stubs([(1, BROKEN_PIN)])
    check("the note fired on a recognised broken pin", "has stopped working" not in error)

    # 4. A compile error is not a proxy problem. Fail fast.
    _, error, calls, _ = with_stubs([(1, COMPILE_ERROR)])
    check("a compile error was retried", len(calls) == 1)
    check("the compile error was swallowed", error and "undefined: doesNotExist" in error)

    # 5. Exhausted retries surface go's output, not just "failed after 3".
    #    The WHOLE message, not a token from it: go_install embeds `last`
    #    verbatim, so anything less would still pass if the error were
    #    truncated to the line that happens to name the checksum DB. (It read
    #    `"sum.golang.org" in error` until CodeQL flagged the bare host as URL
    #    sanitisation — alerts #58 and #82, the same finding twice as the line
    #    moved. Asserting the full text is both stronger and not that shape.)
    _, error, calls, _ = with_stubs([(1, SUMDB_LAG)], attempts=3)
    check("attempts were not exhausted", len(calls) == 3)
    check("the final error lost go's own output", error and SUMDB_LAG.strip() in error)

    # 6. Already on PATH as a real binary: no install at all.
    with tempfile.TemporaryDirectory() as tmp:
        binary = _write(tmp, "entra-emulator", b"\x7fELF\x02\x01\x01")
        shim = _write(tmp, "shim", b"#!/usr/bin/env bash\nexec goenv exec entra-emulator \"$@\"\n")
        gobin = os.path.join(tmp, "gobin")
        os.makedirs(gobin)

        got = _install_with_path(binary)
        check("a binary on PATH was reinstalled", got == binary)

        # 6b. A SHIM on PATH is not a binary this harness can terminate. It must
        #     not be handed back — the whole failure is that doing so looks like
        #     it worked, right up to the next run failing on a busy port.
        got = _install_with_path(shim, go_bin_dir=gobin)  # gobin is empty
        check("a wrapper script on PATH was handed back as the emulator", got != shim)
        check("rejecting a wrapper did not fall through to installing",
              got == os.path.join("/tmp/w", "entra-emulator" + ei.EXE))

        # 6c. ...but the binary the shim would have run is right there in GOBIN,
        #     so take that rather than paying for an install.
        real = _write(gobin, "entra-emulator" + ei.EXE, b"\xcf\xfa\xed\xfe\x0c\x00\x00\x01")
        got = _install_with_path(shim, go_bin_dir=gobin)
        check("the binary behind the shim was not used", got == real)

        # 6d. The test is the executable IMAGE, not a name or a vendor. Anything
        #     the OS runs directly passes; anything that runs something else on
        #     our behalf does not.
        check("an ELF image was not recognised", ei._is_executable_image(binary) is True)
        check("a Mach-O image was not recognised", ei._is_executable_image(real) is True)
        check("a #! wrapper was mistaken for a binary", ei._is_executable_image(shim) is False)
        check("a missing file was mistaken for a binary",
              ei._is_executable_image(os.path.join(tmp, "gone")) is False)

    # 7. The version is DERIVED from go.mod, which is the other half of this
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
    try:
        main()
    except RetryContractError as e:
        print(f"check_entra_install: FAIL: {e}")
        sys.exit(1)
