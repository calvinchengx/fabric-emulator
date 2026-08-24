"""Contract 4 is only a pass when an out-of-band reader sees the artifact.

The kit exists because four unrelated false greens all had the same shape:
the component that wrote reported success, and nothing looked at where the
bytes actually landed. These tests pin that rule in the harness before any
backend is live, so a later compose job cannot quietly confirm through the
engine's own catalog.
"""
from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]
PROBES = REPO / "e2e" / "conformance" / "probes.py"
RUN = REPO / "e2e" / "conformance" / "run.py"


def _load(path: Path, name: str):
    spec = importlib.util.spec_from_file_location(name, path)
    assert spec and spec.loader
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


probes = _load(PROBES, "conformance_probes")
run = _load(RUN, "conformance_run")


def test_writer_success_and_empty_listing_is_a_fail():
    """The false green: engine said ok, OneLake / TDS saw nothing."""
    result = probes.write_landing(
        writer=lambda: probes.WriteClaim(ok=True),
        reader=lambda: probes.Artifact(found=False),
        expected_location="Tables/gold/orders",
        backend="sail",
    )
    assert result.status == "fail"
    assert "writer reported success" in result.error
    assert "Tables/gold/orders" in result.error
    assert result.pointer.endswith("#4-a-success-claim-must-be-witnessed-by-the-artifact")


def test_writer_success_and_listing_finds_the_table_is_a_pass():
    result = probes.write_landing(
        writer=lambda: probes.WriteClaim(ok=True),
        reader=lambda: probes.Artifact(found=True, location="Tables/gold/orders"),
        expected_location="Tables/gold/orders",
        backend="jvm",
    )
    assert result.status == "pass"
    assert result.error == ""


def test_writer_failure_is_a_fail_with_the_writers_error():
    result = probes.write_landing(
        writer=lambda: probes.WriteClaim(ok=False, error="AnalysisException: no such table"),
        reader=lambda: probes.Artifact(found=False),
        expected_location="Tables/gold/orders",
        backend="warehouse",
    )
    assert result.status == "fail"
    assert result.error == "AnalysisException: no such table"


def test_same_callable_as_writer_and_reader_is_refused():
    """The harness bug that would recreate every false green in docs/38 §4."""
    def both():
        raise AssertionError("must not be called")

    with pytest.raises(probes.SameReaderError, match="must not be the one that confirms"):
        probes.write_landing(
            writer=both,
            reader=both,
            expected_location="Tables/gold/orders",
            backend="sail",
        )


def test_record_marks_warehouse_contracts_1_to_3_not_applicable():
    rows = {r["id"]: r for r in probes.record("warehouse")}
    assert rows["1"]["status"] == "na"
    assert rows["2"]["status"] == "na"
    assert rows["3"]["status"] == "na"
    assert rows["4"]["status"] == "gap"


def test_record_emits_a_gap_with_a_pointer_for_every_required_cell():
    for backend in probes.BACKENDS:
        for row in probes.record(backend):
            if row["status"] == "na":
                continue
            assert row["status"] == "gap"
            assert row["pointer"].startswith("38-framework-conformance.md#")
            assert row["error"]


def test_record_rejects_an_unknown_backend():
    with pytest.raises(ValueError, match="unknown backend"):
        probes.record("flink")


def test_a_live_result_replaces_only_the_contract_it_names():
    live = {4: probes.Result(
        id="4", contract="Write landing", backend="sail", status="pass")}
    rows = {r["id"]: r for r in probes.record("sail", live=live)}
    assert rows["4"]["status"] == "pass"
    assert rows["1"]["status"] == "gap"
    assert rows["5"]["status"] == "gap"


def test_a_live_result_cannot_make_an_inapplicable_cell_applicable():
    """The applicability table is the authority, not the harness.

    A backend that offered a result for a contract it has no surface for would
    otherwise publish a green where the doc says `n/a` — a cell nobody could
    reconcile with the prose.
    """
    live = {1: probes.Result(
        id="1", contract="Context chain", backend="warehouse", status="pass")}
    rows = {r["id"]: r for r in probes.record("warehouse", live=live)}
    assert rows["1"]["status"] == "na"


def test_a_live_result_for_an_unknown_contract_is_refused():
    with pytest.raises(ValueError, match="unknown contract"):
        probes.record("sail", live={9: probes.Result(
            id="9", contract="?", backend="sail", status="pass")})


def _claim(**kw):
    base = dict(ok=True, env_workspace="ws", context_workspace="ws",
                context_lakehouse="lake")
    base.update(kw)
    return probes.ContextClaim(**base)


def test_context_chain_passes_when_every_link_matches_the_control_plane():
    r = probes.context_chain(session=lambda: _claim(), expected_workspace="ws",
                             expected_lakehouse="lake", backend="sail")
    assert r.status == "pass"


def test_context_chain_fails_when_a_single_link_is_empty():
    """A framework stops at the first link that answers, so one broken link is
    a broken chain even when the others are right."""
    r = probes.context_chain(session=lambda: _claim(context_workspace=""),
                             expected_workspace="ws", expected_lakehouse="lake",
                             backend="sail")
    assert r.status == "fail"
    assert "runtime.context[currentWorkspaceId]" in r.error
    assert r.pointer


def test_context_chain_names_every_wrong_link_not_only_the_first():
    r = probes.context_chain(
        session=lambda: _claim(env_workspace="other", context_lakehouse=""),
        expected_workspace="ws", expected_lakehouse="lake", backend="sail")
    assert "env.getWorkspaceId()" in r.error
    assert "defaultLakehouseId" in r.error


def test_context_chain_refuses_a_run_where_the_env_fallback_was_set():
    """The fallback answering correctly is how the broken links stayed
    invisible; a green earned that way would re-create the defect."""
    r = probes.context_chain(session=lambda: _claim(env_fallback_set=True),
                             expected_workspace="ws", expected_lakehouse="lake",
                             backend="sail")
    assert r.status == "fail"
    assert "fallback" in r.error


def test_context_chain_reports_the_sessions_own_error():
    r = probes.context_chain(
        session=lambda: probes.ContextClaim(ok=False, error="no findings written"),
        expected_workspace="ws", expected_lakehouse="lake", backend="sail")
    assert r.status == "fail"
    assert "no findings written" in r.error


def test_applicability_matches_docs_38():
    """A probe-side n/a that the doc does not declare is a second list drifting."""
    spec = importlib.util.spec_from_file_location(
        "check_conformance", REPO / "scripts" / "check_conformance.py")
    assert spec and spec.loader
    cc = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(cc)
    table, errors = cc.load_applicability()
    assert errors == []
    for num, verdicts in table.items():
        for backend, applicability in verdicts.items():
            pair = (num, backend)
            if applicability == "n/a":
                assert pair in probes.NOT_APPLICABLE
            else:
                assert pair not in probes.NOT_APPLICABLE
            if applicability == "control":
                assert pair in probes.CONTROL


def test_committed_matrix_is_what_run_py_renders():
    """CI's --check and this test must see the same bytes."""
    generated = run.render()
    committed = (REPO / "docs" / "conformance-matrix.md").read_text(encoding="utf-8")
    assert committed == generated


def test_every_failing_cell_in_the_committed_matrix_has_a_pointer():
    """Belt: check_conformance.py already requires this; pin it on the artifact."""
    text = (REPO / "docs" / "conformance-matrix.md").read_text(encoding="utf-8")
    cells = 0
    for line in text.splitlines():
        if not line.startswith("|"):
            continue
        for cell in line.strip("|").split("|"):
            if "❌" not in cell:
                continue
            cells += 1
            assert "](" in cell, cell
    # DERIVED, not a magic number. This started as `== 15` and broke the moment
    # a cell went green, which is a test that has to be edited every time the
    # thing it guards improves — and an edit-on-every-change assertion teaches
    # people to edit it without reading it. The claim worth pinning is that the
    # rendered ❌ count agrees with the recorded results, so a matrix cannot
    # show a red the JSON does not have (or hide one it does).
    recorded = 0
    for backend in probes.BACKENDS:
        rows = json.loads(
            (REPO / "e2e" / "conformance" / "out" / f"{backend}.json")
            .read_text(encoding="utf-8"))
        recorded += sum(1 for r in rows if r["status"] in ("gap", "fail"))
    assert cells == recorded, (
        f"the matrix renders {cells} ❌ cells but the committed results hold "
        f"{recorded} — regenerate with e2e/conformance/run.py")


def test_committed_json_names_every_contract_on_every_backend():
    for backend in probes.BACKENDS:
        rows = json.loads(
            (REPO / "e2e" / "conformance" / "out" / f"{backend}.json")
            .read_text(encoding="utf-8"))
        assert [r["id"] for r in rows] == [str(n) for n, _, _ in probes.CONTRACTS]
        assert all(r["backend"] == backend for r in rows)
        four = next(r for r in rows if r["id"] == "4")
        assert four["status"] == "pass", four
        assert four["error"] == ""


def _sig(**kw):
    seen = {"run": ["path", "timeout_seconds", "arguments", "workspace"]}
    seen.update(kw)
    return probes.SignatureClaim(ok=True, seen=seen)


REF = {"run": {"params": ["path", "timeout_seconds", "arguments", "workspace"]}}


def test_signature_shape_passes_when_every_documented_parameter_is_present():
    r = probes.signature_shape(session=lambda: _sig(), reference=REF, backend="sail")
    assert r.status == "pass"


def test_signature_shape_allows_extra_parameters():
    """Accepting a parameter and ignoring it is correct emulation when there is
    nothing to switch; the emulator has one session. Only omission is a signal."""
    r = probes.signature_shape(
        session=lambda: _sig(run=["path", "timeout_seconds", "arguments", "workspace",
                                  "spark_environment", "attach_lakehouse"]),
        reference=REF, backend="sail")
    assert r.status == "pass"


def test_signature_shape_fails_on_a_missing_parameter():
    r = probes.signature_shape(
        session=lambda: _sig(run=["path", "timeout_seconds", "arguments"]),
        reference=REF, backend="sail")
    assert r.status == "fail"
    assert "missing workspace" in r.error


def test_signature_shape_fails_on_a_missing_method():
    """Absence is the case that matters most: a framework reads it and stops."""
    r = probes.signature_shape(
        session=lambda: probes.SignatureClaim(ok=True, seen={}),
        reference=REF, backend="sail")
    assert r.status == "fail"
    assert "absent" in r.error and "run" in r.error


def test_signature_shape_fails_when_documented_parameters_are_out_of_order():
    """Fabric's own examples are positional -- `run("Sample1", 90, {...})`. Right
    names in the wrong order accept that call and do something else with it."""
    r = probes.signature_shape(
        session=lambda: _sig(run=["path", "arguments", "timeout_seconds", "workspace"]),
        reference=REF, backend="sail")
    assert r.status == "fail"
    assert "out of order" in r.error


def test_signature_shape_reports_the_sessions_own_error():
    r = probes.signature_shape(
        session=lambda: probes.SignatureClaim(ok=False, error="no signatures reported"),
        reference=REF, backend="sail")
    assert r.status == "fail"
    assert "no signatures reported" in r.error


def test_every_reference_entry_cites_a_source():
    """A reference assembled from memory is the same defect one tier up: a claim
    about Fabric with nothing behind it."""
    ref = json.loads(
        (REPO / "e2e" / "conformance" / "notebookutils-reference.json")
        .read_text(encoding="utf-8"))
    assert ref["modules_not_yet_covered"], "the scope must be declared, not implied"
    for module, methods in ref["modules"].items():
        assert methods, module
        for name, spec in methods.items():
            assert spec["source"].startswith("https://learn.microsoft.com/"), name
            assert spec["read"], name
            assert spec["params"], name
            # The verbatim line is what a reviewer checks the params against.
            assert spec["verbatim"].startswith(f"{name}("), name


RUNTIMES = {"1.3": {"python": "3.11", "spark": "3.5"}}


def _rt(**kw):
    base = dict(ok=True, declared="1.3", python="3.11.15", spark="3.5.5")
    base.update(kw)
    return probes.RuntimeClaim(**base)


def test_runtime_floor_passes_when_the_image_meets_what_it_declares():
    r = probes.runtime_floor(session=lambda: _rt(), runtimes=RUNTIMES, backend="jvm")
    assert r.status == "pass"


def test_runtime_floor_fails_an_image_that_declares_nothing():
    """A framework targeting Runtime 1.3 has no way to ask whether this is one."""
    r = probes.runtime_floor(session=lambda: _rt(declared=""), runtimes=RUNTIMES,
                             backend="jvm")
    assert r.status == "fail"
    assert "declares no FABRIC_RUNTIME" in r.error


def test_runtime_floor_fails_an_image_below_the_floor_it_declares():
    """The measured case: the overlay shipped 3.8.10 under a Runtime 1.3
    heading, and `notebookutils` needs >= 3.9, so nothing could import it."""
    r = probes.runtime_floor(session=lambda: _rt(python="3.8.10"),
                             runtimes=RUNTIMES, backend="jvm")
    assert r.status == "fail"
    assert "Python 3.11" in r.error and "3.8.10" in r.error


def test_runtime_floor_fails_a_runtime_this_kit_cannot_cite():
    """Declaring 1.4 and being believed would be a claim with no source."""
    r = probes.runtime_floor(session=lambda: _rt(declared="1.4"),
                             runtimes=RUNTIMES, backend="jvm")
    assert r.status == "fail"
    assert "no cited floor" in r.error


def test_runtime_floor_compares_versions_numerically_not_as_strings():
    """`3.8` sorts above `3.11` as text, which would pass the exact image that
    failed. The bug this guards is a comparison, not a version."""
    assert probes._at_least("3.11.15", "3.11")
    assert probes._at_least("3.12.0", "3.11")
    assert not probes._at_least("3.8.10", "3.11")
    assert not probes._at_least("3.9", "3.11")
    # Suffixed and short forms both resolve rather than raising.
    assert probes._at_least("3.11.0rc1", "3.11")
    assert probes._at_least("4", "3.11")


def test_every_runtime_entry_cites_a_source():
    ref = json.loads(
        (REPO / "e2e" / "conformance" / "fabric-runtimes.json")
        .read_text(encoding="utf-8"))
    for version, spec in ref["runtimes"].items():
        assert spec["source"].startswith("https://learn.microsoft.com/"), version
        assert spec["read"] and spec["python"], version


def test_both_images_declare_the_runtime_they_target():
    """The declaration is the contract; without it contract 3 cannot be asked."""
    for path in ("docker/spark-agent/Dockerfile", "docker/spark-runtime/Dockerfile"):
        text = (REPO / path).read_text(encoding="utf-8")
        assert "ENV FABRIC_RUNTIME=" in text, path


EXPECTED_CHILDREN = {"child0": "nb-0", "child1": "nb-1", "child2": "nb-2"}


def _iso(seen):
    return probes.IsolationClaim(ok=True, seen=seen)


def _clean():
    return {m: {"marker": m, "identity": nb} for m, nb in EXPECTED_CHILDREN.items()}


def test_concurrent_isolation_passes_when_each_child_knows_only_itself():
    r = probes.concurrent_isolation(session=lambda: _iso(_clean()),
                                    expected=EXPECTED_CHILDREN, backend="sail")
    assert r.status == "pass"


def test_concurrent_isolation_catches_a_child_reporting_another_childs_identity():
    """The leak this contract exists for. One long-lived agent with a namespace
    per session means anything process-global crosses concurrent runs."""
    seen = _clean()
    seen["child1"]["identity"] = "nb-0"
    r = probes.concurrent_isolation(session=lambda: _iso(seen),
                                    expected=EXPECTED_CHILDREN, backend="sail")
    assert r.status == "fail"
    assert "believes it is nb-0" in r.error


def test_concurrent_isolation_names_a_shared_identity_as_such():
    """N children all reporting one id is already caught per-child; saying it
    once names the leak instead of listing N mismatches."""
    seen = {m: {"marker": m, "identity": "nb-0"} for m in EXPECTED_CHILDREN}
    r = probes.concurrent_isolation(session=lambda: _iso(seen),
                                    expected=EXPECTED_CHILDREN, backend="sail")
    assert "sessions share an identity" in r.error


def test_concurrent_isolation_catches_a_child_writing_another_childs_file():
    seen = _clean()
    seen["child2"]["marker"] = "child1"
    r = probes.concurrent_isolation(session=lambda: _iso(seen),
                                    expected=EXPECTED_CHILDREN, backend="sail")
    assert r.status == "fail"
    assert "wrote another child's file" in r.error


def test_concurrent_isolation_reports_children_that_wrote_nothing():
    """A fan-out where two of three vanished must not read as isolation."""
    seen = {"child0": {"marker": "child0", "identity": "nb-0"}}
    r = probes.concurrent_isolation(session=lambda: _iso(seen),
                                    expected=EXPECTED_CHILDREN, backend="sail")
    assert r.status == "fail"
    assert "2 of 3 children wrote no findings" in r.error


def test_concurrent_isolation_refuses_a_fan_out_that_never_happened():
    r = probes.concurrent_isolation(
        session=lambda: probes.IsolationClaim(ok=False, error="no children were created"),
        expected={}, backend="sail")
    assert r.status == "fail"


def _cred(**kw):
    base = dict(ok=True, lifetime=60, slept=75.0, before_ok=True, after_ok=True)
    base.update(kw)
    return probes.CredentialClaim(**base)


def test_credential_lifetime_passes_when_a_run_outlives_its_token():
    r = probes.credential_lifetime(session=lambda: _cred(), backend="sail")
    assert r.status == "pass"


def test_credential_lifetime_needs_a_working_baseline_first():
    """A session that could never reach OneLake would otherwise 'pass' a check
    that only looked at the second operation."""
    r = probes.credential_lifetime(
        session=lambda: _cred(before_ok=False, before_error="401"), backend="sail")
    assert r.status == "fail"
    assert "BEFORE the wait failed" in r.error


def test_credential_lifetime_refuses_a_wait_shorter_than_the_token():
    """THE defect: a token minted at container start, an hour later every read
    401s. A probe that slept less than the token lived would pass on a runtime
    that never re-mints — which is the thing under test."""
    r = probes.credential_lifetime(session=lambda: _cred(slept=30.0), backend="sail")
    assert r.status == "fail"
    assert "gap was never opened" in r.error


def test_credential_lifetime_refuses_a_run_that_reported_no_lifetime():
    r = probes.credential_lifetime(session=lambda: _cred(lifetime=0), backend="sail")
    assert r.status == "fail"
    assert "no token lifetime" in r.error


def test_credential_lifetime_fails_when_the_second_read_401s():
    r = probes.credential_lifetime(
        session=lambda: _cred(after_ok=False, after_error="401 Unauthorized"),
        backend="sail")
    assert r.status == "fail"
    assert "past a 60s token lifetime" in r.error and "401" in r.error


def test_credential_lifetime_reports_a_session_that_never_ran_it():
    r = probes.credential_lifetime(
        session=lambda: probes.CredentialClaim(ok=False, error="not run: no lifetime set"),
        backend="sail")
    assert r.status == "fail"
    assert "not run" in r.error


# ---------------------------------------------------------------- contract 6
#
# These are new alongside the warehouse column, and they cover the whole probe
# rather than only the branch that column added: contract 6 shipped without any
# unit test of `fall_through`, which its two siblings both had.


def _ft(**kw):
    base = dict(ok=True, recognised_ok=True, unrecognised_ok=False,
                unrecognised_error="cannot plan MERGE")
    base.update(kw)
    return probes.FallThroughClaim(**base)


def test_fall_through_passes_on_the_default_engine():
    r = probes.fall_through(session=lambda: _ft(), backend="sail", control=False)
    assert r.status == "pass"


def test_fall_through_is_vacuous_if_nothing_is_intercepting():
    """The recognised statement must SUCCEED. If the interception is not
    installed, everything falls through and the result means nothing."""
    r = probes.fall_through(
        session=lambda: _ft(recognised_ok=False, recognised_error="OPTIMIZE failed"),
        backend="sail", control=False)
    assert r.status == "fail"
    assert "DOES recognise failed" in r.error


def test_fall_through_refuses_a_failure_that_names_our_own_rewriter():
    """A rewrite that failed is a different defect wearing the same red."""
    r = probes.fall_through(
        session=lambda: _ft(unrecognised_error="delta-rs could not plan this"),
        backend="sail", control=False)
    assert r.status == "fail"
    assert "the agent's own rewriting" in r.error


def test_fall_through_refuses_a_failure_with_no_error_text():
    r = probes.fall_through(
        session=lambda: _ft(unrecognised_error="   "),
        backend="sail", control=False)
    assert r.status == "fail"
    assert "no error text to attribute" in r.error


def test_fall_through_fails_when_the_default_engine_ran_what_it_cannot_plan():
    r = probes.fall_through(
        session=lambda: _ft(unrecognised_ok=True, unrecognised_error=""),
        backend="sail", control=False)
    assert r.status == "fail"
    assert "something rewrote it" in r.error


def test_fall_through_control_engine_must_run_the_statement():
    """The control column is what makes the default engine's red readable."""
    r = probes.fall_through(session=lambda: _ft(unrecognised_ok=True),
                            backend="jvm", control=True)
    assert r.status == "pass"
    r = probes.fall_through(session=lambda: _ft(unrecognised_ok=False),
                            backend="jvm", control=True)
    assert r.status == "fail"
    assert "control engine could not run" in r.error


def test_fall_through_reports_a_session_that_ran_neither_statement():
    r = probes.fall_through(
        session=lambda: probes.FallThroughClaim(ok=False, error="the session died"),
        backend="sail", control=False)
    assert r.status == "fail"
    assert "the session died" in r.error


def test_fall_through_echo_is_the_witness_on_a_single_engine_surface():
    """The warehouse has no contrasting engine, so the engine returning the
    bytes it was given is what proves nothing rewrote them."""
    sent = "WITH a AS (WITH b AS (SELECT 1 x) SELECT x FROM b) SELECT x FROM a"
    r = probes.fall_through(
        session=lambda: _ft(echo_sent=sent, echo_got=sent),
        backend="warehouse", control=False)
    assert r.status == "pass"


def test_fall_through_echo_fails_when_the_bytes_came_back_changed():
    sent = "WITH a AS (WITH b AS (SELECT 1 x) SELECT x FROM b) SELECT x FROM a"
    r = probes.fall_through(
        session=lambda: _ft(echo_sent=sent, echo_got="WITH b AS (SELECT 1 x) ..."),
        backend="warehouse", control=False)
    assert r.status == "fail"
    assert "echoed back text the session did not send" in r.error


def test_fall_through_echo_replaces_the_control_contrast_rather_than_adding_to_it():
    """Requiring both would make the single-engine surface unprovable. An echo
    that round-trips passes even though the unrecognised statement 'succeeded',
    which on a two-engine surface would be the failure."""
    sent = "SELECT 1"
    r = probes.fall_through(
        session=lambda: _ft(unrecognised_ok=True, echo_sent=sent, echo_got=sent),
        backend="warehouse", control=False)
    assert r.status == "pass"


# ---------------------------------------------------------------- contract 7


def test_credential_lifetime_refuses_a_surface_that_accepts_an_expired_token():
    """'The run outlived its token' and 'nothing checks credentials' are the
    same green, and the second is a hole wearing the first's result."""
    r = probes.credential_lifetime(
        session=lambda: _cred(expiry_checked=True, expiry_accepted=True),
        backend="warehouse")
    assert r.status == "fail"
    assert "already-expired credential was ACCEPTED" in r.error


def test_credential_lifetime_passes_when_expiry_is_enforced():
    r = probes.credential_lifetime(
        session=lambda: _cred(expiry_checked=True, expiry_accepted=False),
        backend="warehouse")
    assert r.status == "pass"


def test_credential_lifetime_grades_a_surface_that_cannot_check_expiry_on_the_wait():
    """Where the stronger form is unavailable the contract stays what it was;
    it is not silently downgraded for the surfaces that DO offer it."""
    r = probes.credential_lifetime(
        session=lambda: _cred(expiry_checked=False, expiry_accepted=True),
        backend="sail")
    assert r.status == "pass"
