#!/usr/bin/env python3
"""Refuse to start when something else already owns a harness port — and name it.

WHY THE GUARD EXISTS
--------------------
Without it, a bind failure is INDISTINGUISHABLE FROM SUCCESS: the child process
dies, wait_healthy then health-checks whatever else is listening, gets its 200,
and the harness proceeds against a stranger's service. That is exactly what
happened — an unrelated compose project published its own entra on 18443, so
tokens were minted by that issuer and the emulator correctly refused them with a
401 at workspace create. The failure looked like a bug in the code under test.

WHY IT NAMES THE HOLDER
-----------------------
The message used to say `docker ps | grep <port>`, because a container was what
had done it the one time anyone investigated. The commoner cause turned out to
be a LEAKED EMULATOR from an earlier run of this same harness, orphaned onto
init — a case where `docker ps` prints nothing and the reader concludes the
message is wrong rather than that the guess was. A port has exactly one holder
and the OS will say who it is, so asking is strictly better than guessing:
`lsof` names the process, and a parent of PID 1 says it outlived whatever
started it, which is the whole diagnosis in one line.

That is worth more than a tidier message. The leak this was written against
(a version-manager shim swallowing SIGTERM, fixed in e2e/entra_install.py) was
silent at the moment it happened and only ever surfaced here. Whatever causes
the next one will be silent too, and this is the place it will show up.

WHY ONE COPY
------------
Fourteen harnesses carried this function, ten of them byte-identical. An
improvement to the diagnosis had to be made fourteen times or it was made in one
place and quietly untrue in thirteen.
"""

import os
import shutil
import socket
import subprocess

DEFAULT_OVERRIDE = "ENTRA_PORT=<free> FABRIC_PORT=<free> python3 <this harness>"

# What proceeding would actually cost. True of every suite that starts an entra;
# a harness with a different listener says what is true of ITS listener instead,
# because a consequence that does not apply is one a reader learns to skip.
DEFAULT_CONSEQUENCE = (
    "  A health check would then pass against the OTHER service and every\n"
    "  token would carry the wrong issuer.")


def _lsof_listener(port):
    """(command, pid) for whatever is LISTENing on `port`, or None.

    Degrades to None rather than raising: this runs on the failure path of a
    harness, and a diagnostic that can itself fail is a second bug reported in
    place of the first. Windows has no lsof, so there the message falls back to
    its generic form.
    """
    lsof = shutil.which("lsof")
    if not lsof:
        return None
    try:
        proc = subprocess.run(
            [lsof, "-nP", f"-iTCP:{port}", "-sTCP:LISTEN", "-F", "cp"],
            capture_output=True, text=True, timeout=5)
    except (OSError, subprocess.SubprocessError):
        return None
    # -F cp emits one field per line, tagged: "p<pid>" then "c<command>".
    pid, command = None, None
    for line in (proc.stdout or "").splitlines():
        if line.startswith("p"):
            pid = line[1:].strip()
        elif line.startswith("c") and pid:
            command = line[1:].strip()
            break
    return (command, pid) if command and pid else None


def _is_one_of_ours(command):
    """Is the holder an emulator a harness like this one started?

    NOT a PPID check. Being parented to init reads like "orphaned by its
    harness" and is the first thing one reaches for, but on macOS every launchd
    daemon has PPID 1 — including the Docker port publisher, which is the
    ORIGINAL cause this guard was written for. That heuristic therefore prints
    "a LEAKED process from an earlier run" with total confidence about the one
    case where it is wrong, which is worse than the guess it replaced.

    The name is the honest signal: every binary these harnesses start is a
    `*-emulator` (entra, fabric, arm, azure-keyvault). If it is one of those, it
    is ours and it is stale, because a live harness would still own its port.
    """
    return command.endswith("-emulator")


def describe_holder(port):
    """A few lines naming whatever holds `port`, or the honest fallback.

    Separate from `require_free_port` so a test can assert the wording of every
    branch without needing a particular listener on a real port.
    """
    found = _lsof_listener(port)
    if not found:
        return (f"  Nothing could be identified as the holder (lsof is unavailable or\n"
                f"  said nothing). The usual causes are a leftover emulator from an\n"
                f"  earlier run and a container publishing the same port\n"
                f"  (`docker ps | grep {port}`).")
    command, pid = found
    kill_hint = f"kill {pid}" if os.name != "nt" else f"taskkill /PID {pid}"
    if _is_one_of_ours(command):
        return (f"  Held by {command} (pid {pid}) — a LEFTOVER emulator from an earlier\n"
                f"  run, not anything running now. Free it:\n"
                f"    {kill_hint}")
    return (f"  Held by {command} (pid {pid}), which is not one of this repo's\n"
            f"  emulators — something else on this machine owns the port (a container\n"
            f"  publisher, most often; check `docker ps | grep {port}`).")


def require_free_port(port, what, override=DEFAULT_OVERRIDE, consequence=DEFAULT_CONSEQUENCE):
    """Refuse to start when something else already owns `port`.

    Checked before anything starts, so the message names the real problem rather
    than a symptom three steps downstream. `override` is the command line this
    harness would accept to move off the port — it differs per suite, and a
    wrong one sends the reader to an environment variable that does nothing.
    `consequence` is what proceeding would cost in THIS harness.
    """
    # CONNECT, do not bind. A bind test is wrong twice over: SO_REUSEADDR lets a
    # 127.0.0.1 bind succeed on macOS while another socket holds 0.0.0.0 (which
    # is exactly how a docker -p publish looks), and without it the test races
    # its own TIME_WAIT. Asking "does anything answer here" has neither problem
    # and is the actual question.
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.settimeout(0.5)
        if s.connect_ex(("127.0.0.1", int(port))) != 0:
            return
    raise SystemExit(
        f"port {port} is already in use, so this harness cannot start its own "
        f"{what}.\n"
        f"{consequence}\n"
        f"{describe_holder(port)}\n"
        f"  Or run on different ports:\n"
        f"    {override}")
