#!/usr/bin/env python3
"""The port guard: that it fires, that it names the holder, and that no harness
has quietly grown its own copy again.

WHY THE LAST ONE IS ASSERTED. This function existed fourteen times, ten of them
byte-identical, and the improvement that prompted the consolidation — naming the
process on the port instead of guessing at Docker — would otherwise have had to
be made fourteen times to be true. Copies do not announce themselves; the next
harness is written by copying the last one, and a duplicate passes every test
the original passes. So the count is the assertion.
"""

import importlib.util
import pathlib
import re
import socket
import sys

REPO = pathlib.Path(__file__).resolve().parents[1]

spec = importlib.util.spec_from_file_location("port_guard", REPO / "e2e" / "port_guard.py")
pg = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pg)


class PortGuardError(AssertionError):
    """Raised instead of exiting, so pytest can drive these assertions too."""


def fail(msg):
    raise PortGuardError(msg)


def check(name, cond):
    if not cond:
        fail(name)


def _free_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _guard_message(port, **kw):
    """The SystemExit text the guard raises against a real listener on `port`."""
    try:
        pg.require_free_port(port, "entra", **kw)
    except SystemExit as e:
        return str(e)
    return ""


def main():
    # 1. A free port is not an error. Nothing else in a harness runs otherwise.
    pg.require_free_port(_free_port(), "entra")

    # 2. A port with a listener on it stops the run — and says who has it. The
    #    listener is this process, so lsof has something real to find and the
    #    message is asserted against a genuine holder rather than a stub.
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as srv:
        srv.bind(("127.0.0.1", 0))
        # A backlog large enough for every probe below: the guard connects and
        # never accepts, so a backlog of 1 would make the SECOND check look like
        # a free port and pass this file vacuously.
        srv.listen(64)
        port = srv.getsockname()[1]

        message = _guard_message(port)
        check("a busy port did not stop the harness", message)
        check("the message does not name the port", str(port) in message)
        check("the message does not name what could not start", "entra" in message)

        # The point of the change: the message must offer the reader something
        # to act on about THIS port, not a guess about containers.
        holder = pg.describe_holder(port)
        if pg.shutil.which("lsof"):
            check("lsof was available and the holder was still not named",
                  re.search(r"pid \d+", holder))
            check("describe_holder's output never reached the message",
                  holder.strip().splitlines()[0].strip() in message)
        else:
            # Degraded, but honestly: it must not claim to know.
            check("the fallback pretends to have identified a holder",
                  "Nothing could be identified" in holder)

        # 2b. The holder here is python, and the guard must NOT call that a
        #     leftover emulator. Confidence about the wrong cause is the defect
        #     this whole change exists to remove, not one to reintroduce facing
        #     the other way — and on macOS the Docker publisher is parented to
        #     init exactly like a real orphan, so PPID cannot be the test.
        check("a process that is not ours was called a leftover emulator",
              "LEFTOVER" not in holder)
        check("a holder that is not ours was named without saying what to look for",
              "docker ps" in holder)

        # 3. The suite-specific halves are the ones a shared function gets wrong:
        #    an override naming a variable this harness does not read sends the
        #    reader somewhere that does nothing.
        custom = _guard_message(port, override="XMLA_PORT=<free> python3 e2e/xmla/run.py",
                                consequence="  the capture listener would be the one that died.")
        check("a suite's own override did not reach the message",
              "XMLA_PORT=<free>" in custom)
        check("a suite's own consequence did not reach the message",
              "capture listener" in custom)
        check("the default consequence leaked into a suite that overrode it",
              "wrong issuer" not in custom)

    # 4. The port is free again now the socket is closed, so the guard must not
    #    latch. A guard that stays angry is one people learn to override.
    pg.require_free_port(port, "entra")

    # 5. Both branches of the one classification, asserted directly: the names
    #    every binary these harnesses start, and things that merely run on the
    #    same machine.
    for ours in ("entra-emulator", "fabric-emulator", "arm-emulator",
                 "azure-keyvault-emulator"):
        check(f"{ours} was not recognised as one of ours", pg._is_one_of_ours(ours))
    for theirs in ("OrbStack Helper", "com.docker.backend", "python3", "nginx"):
        check(f"{theirs} was claimed as one of ours", not pg._is_one_of_ours(theirs))

    # 6. No harness carries its own copy. See the module docstring.
    strays = sorted(
        str(p.relative_to(REPO)) for p in REPO.glob("e2e/**/*.py")
        if p.name != "port_guard.py" and re.search(r"^def require_free_port\(", p.read_text(
            encoding="utf-8", errors="replace"), re.M))
    check(f"these define their own require_free_port instead of importing the shared "
          f"one, so the diagnosis in e2e/port_guard.py is not what they print: {strays}",
          not strays)

    # 7. ...and that the sweep in 6 is looking at anything at all. A glob that
    #    matches nothing passes 6 forever.
    users = [p for p in REPO.glob("e2e/**/*.py")
             if "from port_guard import" in p.read_text(encoding="utf-8", errors="replace")]
    check(f"only {len(users)} harnesses import the shared guard; the sweep in this "
          f"check is probably looking in the wrong place", len(users) >= 10)

    print(f"port guard: PASS (one copy, imported by {len(users)} harnesses)")


if __name__ == "__main__":
    try:
        main()
    except PortGuardError as e:
        print(f"check_port_guard: FAIL: {e}")
        sys.exit(1)
