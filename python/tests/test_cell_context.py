"""The cell-identity export, and what it turns from dead code into a live path.

Nothing in this repo set FABRIC_JOB_ID / FABRIC_CELL_INDEX before it existed —
there were only readers. `storage._forge_attributed` returns None without them,
so an attributed Storage token was NEVER minted on any path, and
`notebookutils.fs` tagged none of its OneLake requests. Both were dead.

What this does not do: attribute Spark's own writes. A `df.write` executes
inside Sail, whose credentials enter through startup env and cannot vary per
cell — storage.py's own module docstring says it ("There is no per-session
credential channel"), and docker/sail/launcher.py restarts sail to rotate them.
"""
import json
import os
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import storage


def _clear():
    for k in ("FABRIC_JOB_ID", "FABRIC_CELL_INDEX"):
        os.environ.pop(k, None)


def test_exports_the_identity_for_the_duration_of_the_statement():
    _clear()
    with storage.cell_context("job-1", 3):
        assert os.environ["FABRIC_JOB_ID"] == "job-1"
        # str, not int: it travels as a header value and a JWT claim.
        assert os.environ["FABRIC_CELL_INDEX"] == "3"
    assert "FABRIC_JOB_ID" not in os.environ
    assert "FABRIC_CELL_INDEX" not in os.environ


def test_cell_zero_is_a_real_cell():
    """`if not cell` would treat index 0 as absent — the FIRST cell of every
    notebook, which is exactly the one most likely to do the reading."""
    _clear()
    with storage.cell_context("job-1", 0):
        assert os.environ["FABRIC_CELL_INDEX"] == "0"


def test_restores_a_previous_identity_rather_than_clearing_it():
    """The agent is long-lived and serves interleaved sessions. Clearing rather
    than restoring would drop the outer cell's identity, so its later I/O would
    attribute to nothing — a lineage edge silently missing."""
    os.environ["FABRIC_JOB_ID"] = "outer"
    os.environ["FABRIC_CELL_INDEX"] = "7"
    with storage.cell_context("inner", 1):
        assert os.environ["FABRIC_JOB_ID"] == "inner"
    assert os.environ["FABRIC_JOB_ID"] == "outer"
    assert os.environ["FABRIC_CELL_INDEX"] == "7"
    _clear()


def test_restores_even_when_the_statement_raises():
    """A failing cell is the common case, not the edge case. Leaking the
    identity past it would attribute the NEXT statement's I/O to the cell that
    just failed — a wrong edge, which reads as real."""
    _clear()
    try:
        with storage.cell_context("job-1", 2):
            raise RuntimeError("cell failed")
    except RuntimeError:
        pass
    assert "FABRIC_JOB_ID" not in os.environ


def test_an_ordinary_livy_statement_sets_nothing():
    """dbt-fabricspark drives the same agent and has no cell identity. Setting
    a placeholder would attribute its I/O to a notebook run that never ran."""
    _clear()
    with storage.cell_context(None, None):
        assert "FABRIC_JOB_ID" not in os.environ
    with storage.cell_context("", None):
        assert "FABRIC_JOB_ID" not in os.environ


def test_the_export_is_what_makes_the_token_forge_fire(monkeypatch):
    """The point of the export. `_forge_attributed` reads the same two vars, so
    without the context it returns None and delta-rs authenticates with an
    unattributed token — which is what shipped until now."""
    calls = {}

    def fake_urlopen(req, *a, **k):
        calls["body"] = json.loads(req.data)

        class R:
            def read(self):
                return b'{"access_token": "forged"}'

            def __enter__(self):
                return self

            def __exit__(self, *_):
                return False

        return R()

    monkeypatch.setattr(storage.urllib.request, "urlopen", fake_urlopen)
    env = {"ENTRA_CLIENT_ID": "c", "ENTRA_FORGE_URL": "https://entra/forge"}

    # Without the context: no claims to carry, so no forge call at all.
    assert storage._forge_attributed(env) is None
    assert not calls

    # With it, using the SAME environment the agent exports into.
    _clear()
    with storage.cell_context("job-9", 4):
        merged = {**env, **os.environ}
        assert storage._forge_attributed(merged) == "forged"
    assert calls["body"]["extraClaims"] == {
        "fabric_job_id": "job-9", "fabric_cell_index": "4"}
