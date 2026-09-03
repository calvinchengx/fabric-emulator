"""Applying an Environment item to the agent, and refusing to fake isolation.

docs/37 §1 states the trap this pins:

    Do not install into the shared agent on every bind without deciding what
    happens when two sessions bind conflicting Environments. Fabric isolates
    them per container; this emulator cannot, and silently letting the last bind
    win would corrupt a dependency tree.

So a second session binding a DIFFERENT environment is refused with a reason,
and one binding the SAME environment is a no-op rather than a reinstall.
"""
import sys
import types
from pathlib import Path

import pytest

AGENT = Path(__file__).resolve().parents[1] / "spark_agent"
sys.path.insert(0, str(AGENT))


@pytest.fixture
def agent(monkeypatch):
    """Load agent.apply_environment without starting Spark or an HTTP server.

    The module builds a SparkSession at import, so the function under test is
    lifted out with its two collaborators stubbed — the install call and the
    session conf — which is all it touches.
    """
    import importlib.util

    src = (AGENT / "agent.py").read_text()
    start = src.index("_environment_applied = {}")
    end = src.index("\nnamespaces = {}")
    module = types.ModuleType("agent_env_under_test")
    module.__dict__.update({"sys": sys, "os": __import__("os")})

    installs = []

    def fake_install(specs, source):
        installs.append((list(specs), source))
        return True, f"installed {len(specs)} package(s)"

    module.__dict__["_install_packages"] = fake_install
    applied_conf = {}
    module.__dict__["spark"] = types.SimpleNamespace(
        conf=types.SimpleNamespace(set=lambda k, v: applied_conf.__setitem__(k, v)))
    exec(compile(src[start:end], "agent-slice", "exec"), module.__dict__)
    module.installs = installs
    module.applied_conf = applied_conf
    _ = importlib.util  # kept: documents that a normal import is deliberately avoided
    return module


def test_packages_and_spark_config_are_applied(agent):
    out = agent.apply_environment({
        "session": "s1", "environment": "env-1",
        "packages": ["anytree", "great-expectations==0.18.8"],
        "sparkConfig": {"spark.sql.shuffle.partitions": 8},
    })
    assert out["applied"] is True
    assert agent.installs == [(["anytree", "great-expectations==0.18.8"], "environment env-1")]
    # Config values are stringified: Spark's conf.set takes strings, and an int
    # reaches it as one rather than raising deep inside the session.
    assert agent.applied_conf == {"spark.sql.shuffle.partitions": "8"}


def test_a_second_session_binding_the_same_environment_is_a_no_op(agent):
    agent.apply_environment({"session": "s1", "environment": "env-1", "packages": ["anytree"]})
    out = agent.apply_environment({"session": "s2", "environment": "env-1", "packages": ["anytree"]})

    assert out["applied"] is True
    # One install, not two: the same environment must not be paid for twice.
    assert len(agent.installs) == 1


def test_a_conflicting_environment_is_refused_with_a_reason(agent):
    agent.apply_environment({"session": "s1", "environment": "env-1", "packages": ["anytree"]})
    out = agent.apply_environment({"session": "s2", "environment": "env-2", "packages": ["numpy"]})

    assert out["applied"] is False
    # The refusal must say WHY, and name the constraint — one process, no
    # per-session isolation — rather than reading as a bug.
    assert "cannot" in out["reason"] and "env-1" in out["reason"]
    assert "docs/37" in out["reason"]
    # And it must NOT have installed: letting the last bind win is the exact
    # corruption this refuses.
    assert len(agent.installs) == 1


def test_an_environment_with_no_name_is_refused(agent):
    out = agent.apply_environment({"session": "s1", "packages": ["anytree"]})
    assert out["applied"] is False
    assert not agent.installs


def test_jars_are_reported_as_skipped_rather_than_silently_dropped(agent):
    # The classpath is fixed at engine start, so a running Connect session cannot
    # take a jar. Saying so beats a notebook failing far from here.
    out = agent.apply_environment({
        "session": "s1", "environment": "env-jar",
        "packages": [], "jars": ["udfs.jar", "extra.jar"]})
    assert out["jarsSkipped"] == 2


def test_a_failed_install_is_reported_not_raised(agent, monkeypatch):
    attempts = []

    def failing(specs, source):
        attempts.append(list(specs))
        return False, "no matching distribution"

    monkeypatch.setitem(agent.__dict__, "_install_packages", failing)
    req = {"session": "s1", "environment": "env-bad", "packages": ["nope==9.9.9"]}
    out = agent.apply_environment(req)
    # A session bind must not die because a package failed; the caller logs it
    # and the notebook meets a clear ModuleNotFoundError later.
    assert out["applied"] is False
    assert "no matching distribution" in out["reason"]

    # Recording the failed attempt is how a retry Completes with the
    # Environment listed and nothing installed. A second bind of the same id
    # must try again, not claim "already installed".
    retry = agent.apply_environment({**req, "session": "s2"})
    assert retry["applied"] is False
    assert "already installed" not in retry["reason"]
    assert "no matching distribution" in retry["reason"]
    assert len(attempts) == 2


def test_a_failed_install_does_not_occupy_the_agent(agent, monkeypatch):
    def install(specs, source):
        if specs == ["nope==9.9.9"]:
            return False, "no matching distribution"
        return True, f"installed {len(specs)} package(s)"

    monkeypatch.setitem(agent.__dict__, "_install_packages", install)
    assert agent.apply_environment({
        "session": "s1", "environment": "env-bad", "packages": ["nope==9.9.9"],
    })["applied"] is False
    # Nothing was installed, so a later bind of a different Environment is
    # still allowed — recording the failure would refuse it as a conflict.
    out = agent.apply_environment({
        "session": "s2", "environment": "env-good", "packages": ["anytree"],
    })
    assert out["applied"] is True
