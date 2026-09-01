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
    monkeypatch.setattr(s.os.path, "isfile", lambda p: True)
    monkeypatch.setattr(s.os, "access", lambda *_: True)
    return tmp_path


def test_a_missing_main_class_is_refused_before_launching(has_submit, monkeypatch):
    called = []
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: called.append(a) or FakeProc(0))
    out = s.submit("", "/x.jar")
    assert out["ok"] is False and "mainClass is required" in out["error"]
    assert not called, "spark-submit was launched for a task with no class to run"


def test_a_missing_jar_is_refused_before_launching(monkeypatch):
    monkeypatch.setattr(s.shutil, "which", lambda _: "/usr/bin/spark-submit")
    monkeypatch.setattr(s.os.path, "isfile", lambda p: p != "/nope.jar")
    monkeypatch.setattr(s.os, "access", lambda *_: True)
    called = []
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: called.append(a) or FakeProc(0))
    out = s.submit("com.acme.Job", "/nope.jar")
    assert out["ok"] is False and "was not staged" in out["error"]
    assert not called


def test_the_exit_code_decides_not_the_absence_of_an_exception(has_submit, monkeypatch):
    """THE ONE THAT MATTERS. spark-submit returning 2 having printed nothing is
    still a failure; reporting success there is the fabrication this repo
    exists to avoid."""
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: FakeProc(2, "", "boom"))
    out = s.submit("com.acme.Job", "/x.jar")
    assert out["ok"] is False and out["exitCode"] == 2
    assert "exited 2" in out["error"] and out["stderr"] == "boom"


def test_a_zero_exit_succeeds_and_carries_the_output(has_submit, monkeypatch):
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: FakeProc(0, "rows=3\n", ""))
    out = s.submit("com.acme.Job", "/x.jar")
    assert out["ok"] is True and out["exitCode"] == 0 and out["stdout"] == "rows=3\n"
    assert out["error"] == ""


def test_the_command_carries_class_conf_and_argv(has_submit, monkeypatch):
    seen = {}

    def fake_run(cmd, **kw):
        seen["cmd"] = cmd
        return FakeProc(0)

    monkeypatch.setattr(s.subprocess, "run", fake_run)
    s.submit("com.acme.Job", "/x.jar", args=["--full", 7], conf={"spark.a": "b"})
    cmd = seen["cmd"]
    assert "--class" in cmd and "com.acme.Job" in cmd
    assert "spark.a=b" in cmd
    # argv follows the jar, as spark-submit requires, and non-strings survive.
    assert cmd[cmd.index("/x.jar") + 1:] == ["--full", "7"]


def test_a_timeout_is_reported_not_swallowed(has_submit, monkeypatch):
    def boom(*a, **k):
        raise subprocess.TimeoutExpired(cmd="spark-submit", timeout=1)

    monkeypatch.setattr(s.subprocess, "run", boom)
    out = s.submit("com.acme.Job", "/x.jar", timeout=1)
    assert out["ok"] is False and out["exitCode"] is None
    assert "did not finish within" in out["error"]


def test_output_is_bounded(has_submit, monkeypatch):
    """A submit that prints a gigabyte must not put it in an activity output."""
    monkeypatch.setattr(s.subprocess, "run", lambda *a, **k: FakeProc(0, "x" * 50000, "y" * 50000))
    out = s.submit("com.acme.Job", "/x.jar")
    assert len(out["stdout"]) == 8192 and len(out["stderr"]) == 8192
