"""`scripts/doctor.sh` against the Windows docker failures it exists to name.

WHY A TEST FOR A SHELL SCRIPT. check_script_test_coverage.py exempts shell from
the one-test-module-per-script rule, and that exemption is about the rule's
message, not about this file being unworthy of one. doctor.sh is the script
whose whole purpose is to turn an unhelpful platform error into a named cause,
and the two cases below are ones it got wrong in the field: issue #650 arrived
as a raw named-pipe failure from `docker pull`, and a windows-containers daemon
read as "ready — run: make up" right up until compose asked for a linux image.
Both are branches that only fire on someone else's machine, so the only way to
drive them is a stub `docker` on PATH.

The stub answers `context show`, `context ls`, `info` and `compose version` --
every docker call doctor.sh makes. Everything else in the report (the shell
tools, python, the port probe) runs for real and is not what is asserted here.
"""
from __future__ import annotations

import pathlib
import shutil
import subprocess
import sys

import pytest

ROOT = pathlib.Path(__file__).resolve().parents[2]
DOCTOR = ROOT / "scripts" / "doctor.sh"

pytestmark = pytest.mark.skipif(
    sys.platform == "win32" or shutil.which("sh") is None,
    # The logic under test is POSIX shell and platform-independent; what is not
    # portable is the fixture -- an extensionless executable text file resolved
    # off PATH is a POSIX convention, and on Windows the stub would not run at
    # all, so the skip is about the stub rather than about the branches.
    reason="needs a POSIX sh and an executable PATH stub",
)

# A daemon that is reachable, linux, and roomy: the shape every assertion here
# varies one field of.
HEALTHY = {
    "context": "default",
    "ostype": "linux",
    "server": "27.3.1 (linux/arm64), 8 cpu, 16000000000 bytes",
    "memtotal": "16000000000",
    "info_rc": 0,
    "contexts": "default unix:///var/run/docker.sock",
    # Per-context reachability for `docker --context <name> info`, as
    # "<name>:<ostype>" pairs. A context absent from this is unreachable, which
    # is the default: the fixtures that care say so.
    "probe": "",
}

STUB = """#!/bin/sh
case "$1" in
  context)
    case "$2" in
      show) printf '%s\\n' "{context}"; exit 0 ;;
      ls)
        # One format: doctor.sh asks for "{{{{.Name}}}} {{{{.DockerEndpoint}}}}" and
        # formats the list it prints itself, from those rows.
        for row in {contexts_printf}; do
          printf '%s\\n' "$row"
        done
        exit 0 ;;
    esac
    exit 0 ;;
  --context)
    # `docker --context <name> info --format '{{{{.OSType}}}}'` -- the per-context
    # probe behind the verdict on each listed alternative. Unreachable is the
    # default here, as it is on a machine where one runtime is installed.
    for pair in {probe_printf}; do
      case "$pair" in
        "$2:"*) printf '%s\\n' "${{pair#*:}}"; exit 0 ;;
      esac
    done
    exit 1 ;;
  info)
    [ "{info_rc}" != "0" ] && {{
      echo 'error during connect: open //./pipe/dockerDesktopLinuxEngine: The system cannot find the file specified.' >&2
      exit 1
    }}
    case "$*" in
      *'{{{{.OSType}}}}'*) printf '%s\\n' "{ostype}" ;;
      *'{{{{.MemTotal}}}}'*) printf '%s\\n' "{memtotal}" ;;
      *) printf '%s\\n' "{server}" ;;
    esac
    exit 0 ;;
  compose) echo v2.29.0; exit 0 ;;
esac
exit 0
"""


def run(tmp_path, **overrides):
    """doctor.sh with a stubbed `docker`, as (exit code, combined output).

    `docker context ls --format '{{.Name}} {{.DockerEndpoint}}'` is the call
    doctor.sh uses to find the active context's endpoint, so the stub emits one
    space-separated "<name> <endpoint>" line per context.
    """
    fields = HEALTHY | overrides
    lines = [line for line in fields["contexts"].splitlines() if line.strip()]
    pairs = fields["probe"].split()
    text = STUB.format(
        contexts_printf=" ".join(f"'{line}'" for line in lines) or "''",
        probe_printf=" ".join(f"'{pair}'" for pair in pairs) or "''",
        **fields,
    )
    bindir = tmp_path / "bin"
    bindir.mkdir()
    stub = bindir / "docker"
    stub.write_text(text, encoding="utf-8")
    stub.chmod(0o755)

    import os

    env = dict(os.environ, PATH=f"{bindir}{os.pathsep}{os.environ['PATH']}")
    done = subprocess.run(
        ["sh", str(DOCTOR)],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
        timeout=120,
    )
    return done.returncode, done.stdout + done.stderr


def test_a_healthy_linux_daemon_is_ready(tmp_path):
    """The baseline, and the non-vacuity guard for everything below: if the stub
    itself failed to answer, every case would 'detect' a problem that is the
    fixture's."""
    rc, out = run(tmp_path)
    assert rc == 0, out
    assert "ready — run: make up" in out
    assert "FAIL" not in out


def test_windows_containers_mode_is_a_blocker(tmp_path):
    """A windows-containers daemon answers `docker info` and reports a version,
    so every reachability check passes. It cannot run one image in this stack
    -- the failure surfaced later as "no matching manifest for windows/amd64",
    which names a manifest rather than an engine mode."""
    rc, out = run(
        tmp_path,
        ostype="windows",
        server="27.3.1 (windows/amd64), 8 cpu, 16000000000 bytes",
    )
    assert rc == 1, out
    assert "serving windows containers" in out
    # The remedy, not just the diagnosis: the switch is a tray item people do
    # not find by reading a manifest error.
    assert "Switch to Linux containers" in out
    assert "-SwitchLinuxEngine" in out


def test_a_linux_daemon_on_windows_is_not_flagged(tmp_path):
    """The OSType check is about the ENGINE, not the host. Docker Desktop on
    Windows serving linux containers is the supported configuration, and a
    check that read the host would fail every correct Windows install."""
    rc, out = run(
        tmp_path,
        context="desktop-linux",
        contexts="desktop-linux npipe:////./pipe/dockerDesktopLinuxEngine",
        server="27.3.1 (linux/amd64), 8 cpu, 16000000000 bytes",
    )
    assert rc == 0, out
    assert "FAIL" not in out


def test_the_stopped_docker_desktop_pipe_says_to_start_it(tmp_path):
    """Issue #650: the pipe exists only while Docker Desktop runs, so "The
    system cannot find the file specified" on it means stopped. The advice that
    was here -- `docker context use <name>` -- is for a different cause, and
    there is nothing else to select on a machine with one context."""
    rc, out = run(
        tmp_path,
        context="desktop-linux",
        contexts="desktop-linux npipe:////./pipe/dockerDesktopLinuxEngine",
        info_rc=1,
    )
    assert rc == 1, out
    assert "is not reachable" in out
    assert "Docker Desktop" in out and "RUNNING" in out
    # The endpoint is quoted back, so the reader can match it against the error
    # their own `docker pull` printed.
    assert "npipe:////./pipe/dockerDesktopLinuxEngine" in out
    # And the advice that was here is not merely demoted but gone: with one
    # context there is nothing to select, so offering a selection at all --
    # even as a placeholder -- sends the reader after the wrong thing.
    assert "only context configured" in out
    assert "docker context use" not in out


def test_an_unreachable_non_desktop_endpoint_still_lists_the_alternatives(tmp_path):
    """The vendor clash the script was written for: the reachable daemon belongs
    to another product. Naming Docker Desktop here would be wrong, and the
    context list is the fix."""
    rc, out = run(
        tmp_path,
        context="colima",
        contexts=(
            "colima unix:///Users/x/.colima/default/docker.sock\n"
            "default unix:///var/run/docker.sock"
        ),
        info_rc=1,
    )
    assert rc == 1, out
    assert "Docker Desktop" not in out
    assert "docker context use <name>" in out
    assert "default  unix:///var/run/docker.sock" in out


def test_an_alternative_that_answers_is_named_rather_than_guessed_at(tmp_path):
    """Listing the other contexts says they exist, which is not the same as
    saying one of them works -- on the vendor-clash machine the fix is a
    specific context, and the script can find out which by asking it."""
    rc, out = run(
        tmp_path,
        context="desktop-linux",
        contexts=(
            "desktop-linux npipe:////./pipe/dockerDesktopLinuxEngine\n"
            "default unix:///var/run/docker.sock"
        ),
        info_rc=1,
        probe="default:linux",
    )
    assert rc == 1, out
    assert "default  unix:///var/run/docker.sock  (answers, linux)" in out
    assert "docker context use default" in out
    # The placeholder is for when nothing answered. Printing both would leave
    # the reader to work out which of the two sentences applies to them.
    assert "docker context use <name>" not in out


def test_an_alternative_serving_windows_containers_is_not_offered(tmp_path):
    """The trap the probe exists for: a context that answers is still the wrong
    answer when its engine cannot run a linux image. Selecting it trades the
    named-pipe error for a manifest error."""
    rc, out = run(
        tmp_path,
        context="desktop-linux",
        contexts=(
            "desktop-linux npipe:////./pipe/dockerDesktopLinuxEngine\n"
            "default npipe:////./pipe/docker_engine"
        ),
        info_rc=1,
        probe="default:windows",
    )
    assert rc == 1, out
    assert "(answers, but serves windows containers)" in out
    assert "docker context use default" not in out
    assert "docker context use <name>" in out
