#!/usr/bin/env python3
"""Install a pinned Go tool for an e2e run — PATH first, `go install` otherwise.

Nine e2e scripts each shelled out to the same `go install` line, which made two
problems nine problems.

WHY THE RETRY EXISTS, AND WHY IT IS NARROW
------------------------------------------
`go install pkg@v0.3.0` does a DEPRECATION LOOKUP against the module's LATEST
version, not the pinned one. So the moment a newer tag is pushed, every pinned
install breaks until the checksum database has fetched that tag — observed on
2026-08-08, when v0.3.1 was tagged at 04:12Z and jobs at 04:39Z failed with:

    loading deprecation for …/entra-emulator: …@v0.3.1: verifying go.mod:
    reading https://sum.golang.org/lookup/…@v0.3.1: 404 Not Found
    server response: not found: …@v0.3.1: invalid version: unknown revision

The pin was fine; the tag existed; sum.golang.org had simply not caught up. It
cleared on its own, and the tempting "fix" — GOPRIVATE or GONOSUMDB — would
disable checksum verification permanently to route around a window measured in
minutes. Nobody removes that afterwards, because nothing fails to remind them.

So the retry is scoped to exactly that failure: it fires when the error names a
version OTHER than the one being installed (the deprecation lookup), or on a
plain transport error. A genuine `unknown revision` on the PIN ITSELF fails
immediately — a blanket retry would turn a real broken pin into three sleeps and
the same failure, which is how a retry becomes a way of not noticing.

WHY THE VERSION IS READ FROM go.mod
-----------------------------------
Each call site used to carry `@v0.3.0` beside the comment "bump this together
with go.mod" — a list a human maintains against another list, with a comment
where the enforcement should be. The version is read out of `go.mod` instead, so
it cannot drift from the module the emulator itself builds against.
"""

import os
import re
import shutil
import subprocess
import time

ENTRA_MODULE = "github.com/calvinchengx/entra-emulator"
EXE = ".exe" if os.name == "nt" else ""

# The signatures of "the module proxy or checksum DB is catching up", as opposed
# to "this pin is wrong". Matched case-insensitively against the failed command's
# combined output.
_TRANSIENT = (
    "loading deprecation",
    "sum.golang.org",
    "proxy.golang.org",
    "connection reset",
    "i/o timeout",
    "timeout awaiting response",
    "tls handshake",
    "503 service unavailable",
    "502 bad gateway",
)


def repo_root(start=None):
    """The repository root, found by walking up to the go.mod that names this
    module — so a script can live at any depth under e2e/."""
    here = os.path.abspath(start or __file__)
    if os.path.isfile(here):
        here = os.path.dirname(here)
    while True:
        candidate = os.path.join(here, "go.mod")
        if os.path.isfile(candidate):
            return here
        parent = os.path.dirname(here)
        if parent == here:
            raise RuntimeError("no go.mod found above " + (start or __file__))
        here = parent


def module_version(module, root=None):
    """The version go.mod pins `module` to. Raises rather than defaulting: a
    silent fallback would install some other version and the run would pass
    while testing the wrong binary."""
    root = root or repo_root()
    with open(os.path.join(root, "go.mod"), encoding="utf-8") as fh:
        text = fh.read()
    match = re.search(r"^\s*" + re.escape(module) + r"\s+(v\S+)\s*$", text, re.MULTILINE)
    if not match:
        raise RuntimeError(f"{module} is not pinned in go.mod — nothing to install")
    return match.group(1)


def _is_transient(output, version):
    lowered = output.lower()
    if not any(sig in lowered for sig in _TRANSIENT):
        return False
    # "unknown revision <the pin>" is a real breakage even though it arrives
    # through the same machinery. Only a lookup naming a DIFFERENT version is
    # the window this retry is for.
    return f"unknown revision {version.lower()}" not in lowered


def go_install(exe_name, package, work_dir, version=None, log=print, attempts=3, delay=10,
               sleep=time.sleep):
    """Return a path to `exe_name`: from PATH if it is there, otherwise
    `go install package@version` into work_dir. `sleep` is injectable so a test
    can prove the retry without waiting for it."""
    found = shutil.which(exe_name)
    if found:
        return found
    if version is None:
        version = module_version(package.split("/cmd/")[0])
    target = f"{package}@{version}"
    log(f"installing {exe_name} ({version})")

    last = ""
    for attempt in range(1, attempts + 1):
        proc = subprocess.run(["go", "install", target],
                              env={**os.environ, "GOBIN": work_dir},
                              capture_output=True, text=True)
        if proc.returncode == 0:
            return os.path.join(work_dir, exe_name + EXE)
        last = (proc.stdout or "") + (proc.stderr or "")
        if attempt == attempts or not _is_transient(last, version):
            break
        log(f"installing {exe_name}: the module proxy or checksum DB is catching up; "
            f"retrying in {delay}s (attempt {attempt}/{attempts})")
        sleep(delay)

    # The output is printed rather than swallowed: capture_output means the
    # caller has seen nothing, and the whole point of the narrow retry is that a
    # real failure still reads like one.
    raise RuntimeError(f"go install {target} failed:\n{last.strip()}")


def ensure_entra_emulator(work_dir, log=print):
    """The entra-emulator binary, at the version go.mod pins."""
    return go_install("entra-emulator", ENTRA_MODULE + "/cmd/entra-emulator", work_dir, log=log)
