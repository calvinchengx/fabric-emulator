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


def test_live_write_replaces_only_contract_4():
    def live():
        return probes.Result(
            id="4", contract="Write landing", backend="sail", status="pass")

    rows = {r["id"]: r for r in probes.record("sail", live_write=live)}
    assert rows["4"]["status"] == "pass"
    assert rows["1"]["status"] == "gap"
    assert rows["5"]["status"] == "gap"


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
    assert cells == 15  # contract 4 is live ✅; the other required/control cells are gaps


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
