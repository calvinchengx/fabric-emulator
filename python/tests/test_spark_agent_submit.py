"""The submit guards, which decide whether a main class ran.

`submit.py` shells out to spark-submit, so the SUBMISSION cannot be exercised
here — but its guards can, and they are the load-bearing part: whether an engine
reports itself capable, and whether a non-zero exit is allowed to read as
success. Omitting the file for the sake of the part that needs Spark would take
those with it.
"""
import pathlib
import subprocess
import sys
import threading

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "spark_agent"))

import submit as s  # noqa: E402


class FakeProc:
    def __init__(self, rc, out="", err=""):
        self.returncode, self.stdout, self.stderr = rc, out, err


def test_an_engine_without_spark_submit_reports_unavailable(monkeypatch):
    """Sail's answer. `available: False` is a different fact from a failed
    submission, and the emulator relays it as a different refusal."""
    monkeypatch.setattr(s.os.path, "isfile", lambda _: False)
    monkeypatch.setattr(s.shutil, "which", lambda _: None)
    out = s.submit("com.acme.Job", "/lakehouse/default/Files/x.jar")
    assert out["available"] is False and out["ok"] is False
    assert "no spark-submit" in out["error"]


@pytest.fixture
def has_submit(monkeypatch, tmp_path):
    """An engine with spark-submit and a real jar inside the mount.

    A REAL path, not a stub: the containment guard resolves what it is given,
    so a fixture handing out `/x.jar` would exercise the guard rather than the
    behaviour each test is about. Returns the jar's path.
    """
    root = tmp_path / "Files"
    (root / "jobs").mkdir(parents=True)
    jar = root / "jobs" / "etl.jar"
    jar.write_text("PK")
    monkeypatch.setattr(s, "MOUNT_ROOT", str(root))
    monkeypatch.setattr(s, "SPARK_BIN", str(tmp_path / "spark-submit"))
    (tmp_path / "spark-submit").write_text("#!/bin/sh\n")
    (tmp_path / "spark-submit").chmod(0o755)
    return str(jar)


def test_a_missing_main_class_is_refused_before_launching(has_submit, monkeypatch):
    called = []
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: called.append(a) or FakeProc(0))
    out = s.submit("", has_submit)
    assert out["ok"] is False and "mainClass is required" in out["error"]
    assert not called, "spark-submit was launched for a task with no class to run"


def test_a_missing_jar_is_refused_before_launching(has_submit, monkeypatch):
    """A name the enumeration does not produce. Under the new shape this is the
    SAME fact as a traversal — the walk found nothing matching — and saying so
    once is more honest than two messages for one cause."""
    called = []
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: called.append(a) or FakeProc(0))
    out = s.submit("com.acme.Job", s.MOUNT_ROOT + "/jobs/absent.jar")
    assert out["ok"] is False and "no jar matching" in out["error"]
    assert not called


def test_the_exit_code_decides_not_the_absence_of_an_exception(has_submit, monkeypatch):
    """THE ONE THAT MATTERS. spark-submit returning 2 having printed nothing is
    still a failure; reporting success there is the fabrication this repo
    exists to avoid."""
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: FakeProc(2, "", "boom"))
    out = s.submit("com.acme.Job", has_submit)
    assert out["ok"] is False and out["exitCode"] == 2
    assert "exited 2" in out["error"] and out["stderr"] == "boom"


def test_a_zero_exit_succeeds_and_carries_the_output(has_submit, monkeypatch):
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: FakeProc(0, "rows=3\n", ""))
    out = s.submit("com.acme.Job", has_submit)
    assert out["ok"] is True and out["exitCode"] == 0 and out["stdout"] == "rows=3\n"
    assert out["error"] == ""


def test_the_command_carries_class_conf_and_argv(has_submit, monkeypatch):
    seen = {}

    def fake_run(cmd, **kw):
        seen["cmd"] = cmd
        return FakeProc(0)

    monkeypatch.setattr(s.subprocess, "run", fake_run)
    s.submit("com.acme.Job", has_submit, args=["--full", 7], conf={"spark.a": "b"})
    cmd = seen["cmd"]
    assert "--class" in cmd and "com.acme.Job" in cmd
    assert "spark.a=b" in cmd
    # argv follows the jar, as spark-submit requires, and non-strings survive.
    assert cmd[cmd.index(has_submit) + 1:] == ["--full", "7"]


def test_a_timeout_is_reported_not_swallowed(has_submit, monkeypatch):
    def boom(*a, **k):
        raise subprocess.TimeoutExpired(cmd="spark-submit", timeout=1)

    monkeypatch.setattr(s.subprocess, "run", boom)
    out = s.submit("com.acme.Job", has_submit, timeout=1)
    assert out["ok"] is False and out["exitCode"] is None
    assert "did not finish within" in out["error"]


def test_output_is_bounded(has_submit, monkeypatch):
    """A submit that prints a gigabyte must not put it in an activity output."""
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: FakeProc(0, "x" * 50000, "y" * 50000))
    out = s.submit("com.acme.Job", has_submit)
    assert len(out["stdout"]) == 8192 and len(out["stderr"]) == 8192


def test_a_path_that_leaves_the_mount_is_refused(has_submit, monkeypatch):
    """CodeQL's py/path-injection, and it was right: the agent is a service, so
    this path is only as trusted as the port it arrived on. Traversal out of the
    lakehouse mount must be refused, not handed to spark-submit."""
    called = []
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: called.append(a) or FakeProc(0))
    out = s.submit("com.acme.Job", s.MOUNT_ROOT + "/../../opt/evil.jar")
    assert out["ok"] is False and "no jar matching" in out["error"]
    assert not called, "spark-submit was handed a path outside the mount"


def test_an_absolute_path_elsewhere_is_refused(has_submit, monkeypatch):
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: FakeProc(0))
    out = s.submit("com.acme.Job", "/etc/passwd")
    assert out["ok"] is False and "no jar matching" in out["error"]


def test_a_symlink_out_of_the_mount_is_refused(tmp_path, monkeypatch):
    root = tmp_path / "Files"
    (root / "jobs").mkdir(parents=True)
    outside = tmp_path / "outside.jar"
    outside.write_text("outside")
    try:
        (root / "jobs" / "etl.jar").symlink_to(outside)
    except OSError as exc:
        pytest.skip(f"symlinks are unavailable on this runner: {exc}")
    monkeypatch.setattr(s, "MOUNT_ROOT", str(root))
    assert s._resolve_in_mount("jobs/etl.jar") is None


def test_a_sibling_directory_sharing_the_prefix_is_refused(tmp_path, monkeypatch):
    """`/lakehouse/default/Files-evil` starts with the root AS A STRING while
    being a different directory. The enumeration never produces it, so the
    prefix trap cannot be fallen into rather than being guarded against."""
    root = tmp_path / "Files"
    root.mkdir()
    evil = tmp_path / "Files-evil"
    evil.mkdir()
    (evil / "x.jar").write_text("nope")
    monkeypatch.setattr(s, "MOUNT_ROOT", str(root))
    assert s._resolve_in_mount(str(evil / "x.jar")) is None


def test_a_jar_inside_the_mount_is_accepted(tmp_path, monkeypatch):
    root = tmp_path / "Files"
    (root / "jobs").mkdir(parents=True)
    jar = root / "jobs" / "etl.jar"
    jar.write_text("PK")
    monkeypatch.setattr(s, "MOUNT_ROOT", str(root))
    assert s._resolve_in_mount(str(jar)) == str(jar.resolve())
    # ...and the guard is not simply refusing everything.
    monkeypatch.setattr(s.shutil, "which", lambda _: "/usr/bin/spark-submit")
    monkeypatch.setattr(s.os.path, "isfile", lambda p: True)
    monkeypatch.setattr(s.os, "access", lambda *_: True)
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: FakeProc(0, "ok\n"))
    assert s.submit("com.acme.Job", str(jar))["ok"] is True


def test_a_backslash_request_matches_the_same_jar(tmp_path, monkeypatch):
    """The Windows failure, pinned. `os.path.relpath` yields `jobs\\etl.jar`
    there while the request carries `jobs/etl.jar`; a comparison that depends
    on the host's separator made every jar look absent on one runner and not
    the other."""
    root = tmp_path / "Files"
    (root / "jobs").mkdir(parents=True)
    jar = root / "jobs" / "etl.jar"
    jar.write_text("PK")
    monkeypatch.setattr(s, "MOUNT_ROOT", str(root))
    for form in ("jobs/etl.jar", "jobs\\etl.jar", str(jar), str(jar).replace("/", "\\")):
        assert s._resolve_in_mount(form) == str(jar.resolve()), form


def test_submit_holds_the_mount_lock_while_the_jvm_runs(has_submit, monkeypatch):
    """A statement refresh must wait, not rewrite Files/ under spark-submit."""
    import files_mount
    blocked = []
    waiter = []

    def run(*_a, **_k):
        t = threading.Thread(target=files_mount.refresh)
        waiter.append(t)
        t.start()
        t.join(0.15)
        blocked.append(t.is_alive())
        return FakeProc(0, "ok\n")

    monkeypatch.setattr(s.subprocess, "run", run)
    assert s.submit("com.acme.Job", has_submit)["ok"] is True
    assert blocked == [True], "refresh ran while spark-submit held the mount"
    waiter[0].join(2)
