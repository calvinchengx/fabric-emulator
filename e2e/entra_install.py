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

WHY A PATH HIT IS NOT ENOUGH
----------------------------
"PATH first" also has to mean "a path the harness can shut down". A version
manager puts a SHIM on PATH, and a shim runs the real binary as a child — so
every teardown in e2e/ signalled the shim and left the emulator holding its
port. See `_direct_binary`, which is where that is now caught.
"""

import os
import re
import shutil
import subprocess
import time

ENTRA_MODULE = "github.com/calvinchengx/entra-emulator"
EXE = ".exe" if os.name == "nt" else ""

# Any version string the toolchain names in an error, e.g. "…@v0.3.1:".
_VERSION_IN_ERROR = re.compile(r"@(v\d+\.\d+\.\d+[^\s:,)]*)")

# The hosts that mean "module resolution was involved". Hostnames rather than
# prose: Go rewords its messages between releases, and a check that depends on
# its wording is a second list maintained against someone else's changelog.
_MODULE_INFRA = ("sum.golang.org", "proxy.golang.org")

# Transport failures, which are transient whatever they are talking about.
_TRANSIENT = (
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
    """Is this the "a newer tag is still propagating" window, or a real failure?

    THE PRIMARY TEST IS STRUCTURAL, not textual. The deprecation lookup targets
    the module's LATEST version, so its failure necessarily names a version
    OTHER than the one being installed — and that is true whatever words Go
    wraps around it. Matching the prose instead ("loading deprecation ...")
    would be a second list maintained against Go's changelog, with nothing to
    notice if a release reworded it: the retry would silently stop firing and
    this whole class would come back wearing the toolchain's clothes.

    Do NOT "simplify" this into matching a fixed version string. The other side
    of the comparison is whatever was tagged minutes ago, which this code cannot
    know in advance; only the PINNED side is knowable, so the test has to be
    "some version other than the pin".
    """
    lowered = output.lower()
    if any(sig in lowered for sig in _TRANSIENT):
        return True
    if not any(host in lowered for host in _MODULE_INFRA):
        return False
    # A failure naming the pin and nothing else is a genuinely broken pin: same
    # machinery, opposite meaning, and retrying it would bury the diagnosis.
    others = set(_VERSION_IN_ERROR.findall(lowered)) - {version.lower()}
    return bool(others)


def _unclassified_note(output, version):
    """The warning for a module-resolution failure this code did not recognise.

    Suggested by the parity session, and it closes the residual hole: if Go
    changes how these errors read, the retry stops firing and the only symptom
    is that the old breakage is back. Saying so at the point of failure turns an
    invisible regression into a diagnosable one, without widening the retry.
    """
    lowered = output.lower()
    if not any(host in lowered for host in _MODULE_INFRA):
        return ""
    # A failure that names versions was CLASSIFIED — as the propagation window
    # if it named another one, as a broken pin if it named only this one. The
    # note is for the residual case where the structural test had nothing to
    # work with, which is what a reworded message that stops naming versions
    # would look like.
    if _VERSION_IN_ERROR.search(lowered):
        return ""
    return (
        "\nNOTE: this failed during module resolution but did not match the known\n"
        "propagation window (an error naming a version other than the pinned "
        + version + ").\n"
        "Either it is a real failure, or the toolchain reworded these errors and the\n"
        "retry in e2e/entra_install.py has stopped working. Check which before\n"
        "assuming the first."
    )


def _is_executable_image(path):
    """Does the OS run this file itself, or does something else run it for us?

    The magic bytes of every format a Go build produces. A file that starts with
    anything else — `#!`, most obviously — is a wrapper: running it starts one
    process that starts another, and only the outer one is ours to signal.
    """
    try:
        with open(path, "rb") as fh:
            head = fh.read(4)
    except OSError:
        return False
    return head.startswith((
        b"\x7fELF",            # Linux
        b"MZ",                 # Windows PE
        b"\xcf\xfa\xed\xfe",   # Mach-O 64-bit, little-endian (arm64 and amd64)
        b"\xce\xfa\xed\xfe",   # Mach-O 32-bit
        b"\xca\xfe\xba\xbe",   # Mach-O universal ("fat")
        b"\xbe\xba\xfe\xca",   # ... and byte-swapped
    ))


def _go_bin_dir():
    """Where `go install` puts binaries on this machine, or None.

    This is where the shim's own target lives, because the tool got onto PATH by
    being `go install`ed in the first place.
    """
    try:
        proc = subprocess.run(["go", "env", "GOBIN", "GOPATH"], capture_output=True, text=True)
    except OSError:
        return None  # no toolchain; go_install is about to say so properly
    if proc.returncode != 0:
        return None
    lines = (proc.stdout or "").splitlines()
    gobin = lines[0].strip() if len(lines) > 0 else ""
    gopath = lines[1].strip() if len(lines) > 1 else ""
    if gobin:
        return gobin
    return os.path.join(gopath, "bin") if gopath else None


def _direct_binary(exe_name, log=print):
    """A path to `exe_name` that a harness can actually terminate, or None.

    THE BUG THIS EXISTS FOR. `shutil.which` returns whatever PATH resolves, and
    under a version manager what it resolves to is a SHIM: a small script whose
    last line is `exec goenv exec entra-emulator`. goenv is itself a Go program,
    and it runs the requested tool with os/exec rather than replacing itself
    with it, so the process the harness holds is the middle one:

        python (Popen) -> goenv exec entra-emulator -> entra-emulator (LISTEN)

    `terminate()` then reaches goenv, `wait()` returns -15, and every teardown in
    e2e/ REPORTS A CLEAN SHUTDOWN while the emulator is reparented to init and
    keeps its port for as long as the machine is up. Nothing fails at the time.
    The next run fails instead, in require_free_port, three steps from the cause
    — which is how seven of these accumulated on one laptop, one of them from a
    Go toolchain two versions old, before anyone traced the port back to a
    process rather than to a container.

    So the test is "can the OS run this image directly", not "does this look like
    goenv". A wrapper's signal behaviour cannot be read off disk: a wrapper that
    ends in a real `exec` would be fine and one that forks is not, they are
    indistinguishable without running them, and the cost of guessing wrong is
    silent. Refusing every wrapper costs a `go install` that is almost always a
    cache hit; accepting one costs a leaked port and a misdirected diagnosis.
    """
    found = shutil.which(exe_name)
    if not found:
        return None
    if _is_executable_image(found):
        return found

    # PATH gave us a wrapper. The binary it would have run was `go install`ed,
    # so look where that puts things and take the image directly.
    bin_dir = _go_bin_dir()
    direct = os.path.join(bin_dir, exe_name + EXE) if bin_dir else ""
    if direct and _is_executable_image(direct):
        log(f"{found} is a wrapper script, which a harness cannot signal; "
            f"using {direct} instead")
        return direct
    log(f"{found} is a wrapper script, which a harness cannot signal, and no "
        f"binary was found in {bin_dir or 'GOBIN/GOPATH'}; installing our own copy")
    return None


def go_install(exe_name, package, work_dir, version=None, log=print, attempts=3, delay=10,
               sleep=time.sleep):
    """Return a path to `exe_name`: from PATH if it is there AS A BINARY THIS
    HARNESS CAN TERMINATE (see `_direct_binary`), otherwise
    `go install package@version` into work_dir. `sleep` is injectable so a test
    can prove the retry without waiting for it."""
    found = _direct_binary(exe_name, log=log)
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
    raise RuntimeError(f"go install {target} failed:\n{last.strip()}"
                       + _unclassified_note(last, version))


def ensure_entra_emulator(work_dir, log=print):
    """The entra-emulator binary, at the version go.mod pins."""
    return go_install("entra-emulator", ENTRA_MODULE + "/cmd/entra-emulator", work_dir, log=log)
