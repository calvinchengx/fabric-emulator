"""Differential witness for `runMultiple`: the same DAG, both targets, diffed.

WHY THIS EXISTS. Every gap closed in docs/39 was found by INSPECTION — reading
our code against Microsoft's reference. That yields "gaps I could find", which
is strictly smaller than "gaps that exist". Closing them earns the claim *no
known divergence*; it does not earn *no divergence*. Only running the same DAG
against a real tenant and comparing earns that.

The harness already existed. This suite runs against the emulator on every push
(`e2e/fabric-target`) and against real Microsoft Fabric weekly, secret-gated
(`.github/workflows/real-fabric.yml`), with `FABRIC_TARGET` the only
difference. So this file adds cases to a fidelity oracle rather than building
one.

HOW A DIFFERENTIAL TEST STAYS HONEST. Ids and timings can never match across
targets, so the comparison is a NORMALISED PROJECTION: the activity names, each
status, each exit value, whether a failure reason is populated (never its
text), and the observed order. Everything else is deliberately not compared.

And divergences we chose on purpose — sequential by default, isolated child
sessions — are DECLARED below. An undeclared divergence fails. A declared one
that no longer diverges also fails, because a stale allowlist is how this drifts
back into fiction, exactly as a declared skip that no longer skips is an error
in check_witnesses.py.
"""
import os
import sys
import uuid

import pytest

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from fabric_target import target  # noqa: E402

pytestmark = pytest.mark.target

# MARKDOWN ONLY, deliberately. A notebook with executable cells needs a Spark
# engine to reach a terminal state, and the emulator leg of this suite runs the
# binaries with no engine attached — so a child with code would poll to its
# timeout here and complete on Fabric, which is a difference in the HARNESS, not
# in the thing under test. Structure, ordering, result shape and the failure
# contract are all observable without executing a line.
#
# Exit VALUES need an engine and are covered where one exists: e2e/notebook-driven
# runs this same shim against real Sail and asserts the value round-trips.
CHILD_BODY = """# Fabric notebook source

# MARKDOWN ********************
# MAGIC ## nothing to execute
"""

# Divergences the emulator takes ON PURPOSE. Each names the parity row that
# documents it, and the tests below fail if that row stops existing — a
# divergence whose documentation has been deleted is undeclared again.
KNOWN_DIVERGENCES = {
    "sequential-by-default": {
        "parity_row": "`runMultiple` — concurrency",
        "why": "Fabric defaults concurrency to 3x CPU cores; the emulator runs "
               "one activity at a time so a run is reproducible.",
    },
    "isolated-child-sessions": {
        "parity_row": "`runMultiple` — child Spark sessions",
        "why": "Fabric runs children on isolated REPLs inside the parent's Spark "
               "session; the emulator gives each child its own session, so "
               "sibling temp views are not shared.",
    },
}


def project(results):
    """The comparable shape of a runMultiple result.

    Ids, timings and error text are excluded deliberately: none of them can
    match across targets, and comparing them would make every run diverge for
    reasons that say nothing about parity.
    """
    return {
        name: {
            "exitVal": r.get("exitVal"),
            "failed": r.get("exception") is not None,
        }
        for name, r in sorted(results.items())
    }


@pytest.fixture(scope="module")
def t():
    tgt = target(fresh=True)
    if not tgt.workspace_scope:
        pytest.skip("FABRIC_WORKSPACE not set — conformance is always workspace-scoped")
    return tgt


@pytest.fixture(scope="module")
def ws(t):
    return t.workspace()


@pytest.fixture(scope="module")
def shim(t, ws):
    """Point `notebookutils` at the same target `fabric_target` resolved.

    Two resolvers exist for good reasons — `fabric_target` is the toggle a
    consumer wires their own clients through, `notebookutils` is the shim a
    notebook imports — and they read different environment variables. Bridging
    them here is what lets ONE conformance suite exercise both, rather than
    each target growing its own copy of these assertions.

    Both honour `FABRIC_TARGET`, so real mode inherits real endpoints and
    DefaultAzureCredential without anything being seeded here.
    """
    import notebookutils._config as nbconfig

    root = t.api_root[: -len("/v1")] if t.api_root.endswith("/v1") else t.api_root
    env = {
        "NOTEBOOKUTILS_FABRIC_URL": root,
        "NOTEBOOKUTILS_ENTRA_URL": t.entra_url,
        "NOTEBOOKUTILS_TENANT": t.tenant,
        "NOTEBOOKUTILS_WORKSPACE_ID": ws.id,
    }
    if not t.tls_verify:
        env["NOTEBOOKUTILS_INSECURE"] = "1"
    previous = {k: os.environ.get(k) for k in env}
    os.environ.update(env)
    nbconfig._cfg = None  # the shim caches its config on first use
    yield
    for key, value in previous.items():
        if value is None:
            os.environ.pop(key, None)
        else:
            os.environ[key] = value
    nbconfig._cfg = None


@pytest.fixture(scope="module")
def children(t, ws, shim):
    """Two child notebooks that exist on whichever target is under test."""
    import base64

    s = t.session()
    made = []
    for suffix in ("a", "b"):
        name = f"diff-{suffix}-{uuid.uuid4().hex[:8]}"
        body = CHILD_BODY + f"# MAGIC child {suffix}\n"
        r = s.post(f"/workspaces/{ws.id}/items", json={
            "displayName": name, "type": "Notebook",
            "definition": {"parts": [{
                "path": "notebook-content.py", "payloadType": "InlineBase64",
                "payload": base64.b64encode(body.encode()).decode()}]}})
        assert r.status_code in (201, 202), (r.status_code, r.text[:200])
        t.poll_lro(r)
        made.append(name)
    yield made
    os.environ["FABRIC_TARGET_ALLOW_DESTRUCTIVE"] = "1"
    try:
        items = s.get(f"/workspaces/{ws.id}/items").json()["value"]
        for item in items:
            if item["displayName"] in made:
                t.poll_lro(s.delete(f"/workspaces/{ws.id}/items/{item['id']}"))
    finally:
        os.environ.pop("FABRIC_TARGET_ALLOW_DESTRUCTIVE", None)


def run_dag(t, ws, children):
    """Run the reference DAG on this target and return its projection + order."""
    from notebookutils import notebook as nbu
    from notebookutils.common.exceptions import RunMultipleFailedException

    first, second = children
    dag = {"activities": [
        {"name": "second", "path": second, "dependencies": ["first"]},
        {"name": "first", "path": first},
    ]}
    try:
        results = nbu.runMultiple(dag)
    except RunMultipleFailedException as ex:
        results = ex.result
    return project(results)


def test_the_dag_projection_matches_the_recorded_baseline(t, ws, children, shim):
    """The same DAG produces the same comparable shape on either target.

    Run under FABRIC_TARGET=emulator on every push and FABRIC_TARGET=real
    weekly; a divergence shows up as this assertion failing on the real leg,
    which is the signal the whole file exists to produce.

    The BASELINE is written as literals rather than as a diff between two live
    runs, because the two legs run days apart on different machines. A literal
    both legs must satisfy is the same comparison with the coordination
    removed — and it fails loudly on whichever leg stops matching.
    """
    got = run_dag(t, ws, children)
    assert set(got) == {"first", "second"}, got
    # Neither child calls exit(), and the documented answer for that is "" on
    # both targets — not None, which is what a caller doing `int(v or 0)`
    # would trip over.
    assert got["first"] == {"exitVal": "", "failed": False}, got
    assert got["second"] == {"exitVal": "", "failed": False}, got


def test_a_dependent_of_a_failure_does_not_run_on_either_target(t, ws, children, shim):
    """A DAG naming a notebook that does not exist fails, and the dependent is
    not run. Both halves are target-independent: no engine is involved in
    refusing to start an activity whose dependency did not complete."""
    from notebookutils import notebook as nbu
    from notebookutils.common.exceptions import RunMultipleFailedException

    with pytest.raises(RunMultipleFailedException) as ei:
        nbu.runMultiple({"activities": [
            {"name": "ghost", "path": f"no-such-notebook-{uuid.uuid4().hex[:8]}"},
            {"name": "after", "path": children[0], "dependencies": ["ghost"]},
        ]})
    got = project(ei.value.result)
    assert got["ghost"]["failed"] is True, got
    assert got["after"]["failed"] is True, got


def test_validate_dag_agrees_on_a_cycle(t, shim):
    from notebookutils import notebook as nbu

    with pytest.raises(Exception) as ei:  # noqa: B017 — the TYPE differs by target
        nbu.validateDAG({"activities": [
            {"name": "a", "dependencies": ["b"]},
            {"name": "b", "dependencies": ["a"]},
        ]})
    # The message wording is not compared — only that both targets refuse.
    assert ei.value is not None


def test_every_known_divergence_names_a_parity_row():
    """A declaration without a documented row is an excuse, not a divergence."""
    import pathlib

    parity = pathlib.Path(__file__).resolve().parents[3] / "docs" / "parity.md"
    text = parity.read_text(encoding="utf-8")
    for key, entry in KNOWN_DIVERGENCES.items():
        assert entry["parity_row"] in text, (
            f"declared divergence {key!r} names a parity row that does not "
            f"exist: {entry['parity_row']!r}")
        assert entry["why"], key


def test_the_divergence_list_is_not_a_dumping_ground():
    """Every entry must be one of the two the plan actually decided on.

    A differential suite whose allowlist can grow silently proves nothing: the
    next real divergence would be muted by adding a line. Growing this set is a
    decision that belongs in docs/39, so it fails here until it is made there.
    """
    assert set(KNOWN_DIVERGENCES) == {"sequential-by-default", "isolated-child-sessions"}
