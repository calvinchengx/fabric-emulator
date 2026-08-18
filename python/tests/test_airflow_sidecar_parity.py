"""The built-in Airflow sidecar must match what real Fabric runs.

Fabric's Apache Airflow job is upstream Airflow, so "parity" here is checkable
against Microsoft's published configuration rather than inferred. Two of these
properties were WRONG for as long as this sidecar existed, and neither could be
seen by the e2e that exercises it:

  * the executor was `SequentialExecutor`. Microsoft documents Fabric's default
    as `CeleryExecutor` and lists `AIRFLOW__CORE__EXECUTOR` among the settings a
    user CANNOT override -- so on Fabric it is always Celery and no DAG can opt
    out. Sequential runs one task at a time, so a DAG with parallel branches
    serialises, passes, and demonstrates behaviour no Fabric user can have.
  * `DAGS_ARE_PAUSED_AT_CREATION` was "true"; Fabric's default is False.

A ONE-TASK WITNESS DAG CANNOT SEE EITHER. That is why they survived a green
end-to-end test, and why these assertions are made against the file rather than
against a run: the run would have to be a parallel DAG to notice, and the e2e's
is not.

The two compose files are checked TOGETHER because the e2e carries its own copy
of the service. They had already drifted once in the making of this change.
"""
import pathlib
import re

import pytest

ROOT = pathlib.Path(__file__).resolve().parents[2]
SHIPPED = ROOT / "docker-compose.yml"
E2E = ROOT / "e2e" / "airflow" / "docker-compose.yml"

# What Microsoft documents for Fabric's Apache Airflow job:
#   https://learn.microsoft.com/en-us/fabric/data-factory/apache-airflow-jobs-concepts
#   https://learn.microsoft.com/en-us/fabric/data-factory/apache-airflow-jobs-supported-apache-airflow-configurations
FABRIC_AIRFLOW_IMAGE = "apache/airflow:2.10.5-python3.12"
FABRIC_EXECUTOR = "CeleryExecutor"


def airflow_block(path: pathlib.Path) -> str:
    """The `airflow:` service, to the start of the next top-level service.

    Hand-rolled rather than via PyYAML, as the sibling checks in this directory
    are: this repository's tests do not carry a YAML dependency.
    """
    text = path.read_text(encoding="utf-8")
    start = re.search(r"^  airflow:$", text, re.MULTILINE)
    assert start, f"no airflow service in {path}"
    rest = text[start.end():]
    nxt = re.search(r"^  [a-z][a-z0-9-]*:$", rest, re.MULTILINE)
    block = rest[: nxt.start()] if nxt else rest
    # COMMENTS STRIPPED, because these assertions are about configuration and a
    # comment is prose. The first version of this test failed on the sentence
    # explaining WHY SQLite cannot back a parallel executor -- it read the word
    # and concluded the file still used it. An assertion that cannot tell code
    # from the text describing it will fire on its own documentation.
    return "\n".join(
        line for line in block.splitlines() if not line.lstrip().startswith("#")
    )


@pytest.mark.parametrize("path", [SHIPPED, E2E], ids=["shipped", "e2e"])
def test_the_executor_is_the_one_fabric_forces(path):
    block = airflow_block(path)
    assert f"AIRFLOW__CORE__EXECUTOR: {FABRIC_EXECUTOR}" in block, (
        f"{path.name} does not run {FABRIC_EXECUTOR}. Fabric forbids overriding "
        f"AIRFLOW__CORE__EXECUTOR, so an emulator on any other executor is "
        f"offering behaviour no Fabric user can have -- and SequentialExecutor "
        f"specifically serialises every parallel DAG while still passing."
    )
    assert "SequentialExecutor" not in block, f"{path.name} is back on SequentialExecutor"


@pytest.mark.parametrize("path", [SHIPPED, E2E], ids=["shipped", "e2e"])
def test_celery_has_a_broker_and_a_real_metadata_database(path):
    block = airflow_block(path)
    assert "AIRFLOW__CELERY__BROKER_URL" in block, f"{path.name}: Celery with no broker"
    # SQLite cannot serve concurrent writers -- a parallel executor on it fails
    # with `database is locked`, so the database is not a separable choice.
    assert "sqlite" not in block.lower(), (
        f"{path.name} pairs a parallel executor with SQLite, which answers "
        f"`database is locked` under concurrent writers"
    )
    assert "postgresql" in block, f"{path.name}: no Postgres metadata database"
    assert "celery worker" in block, f"{path.name} starts no Celery worker"


@pytest.mark.parametrize("path", [SHIPPED, E2E], ids=["shipped", "e2e"])
def test_the_version_is_the_one_fabric_supports(path):
    """Fabric supports exactly 2.10.5 on Python 3.12, and no Airflow 3.x."""
    assert FABRIC_AIRFLOW_IMAGE in airflow_block(path), (
        f"{path.name} does not run {FABRIC_AIRFLOW_IMAGE}"
    )


@pytest.mark.parametrize("path", [SHIPPED, E2E], ids=["shipped", "e2e"])
def test_dags_are_not_paused_at_creation_as_fabric_leaves_them(path):
    assert 'AIRFLOW__CORE__DAGS_ARE_PAUSED_AT_CREATION: "false"' in airflow_block(path), (
        f"{path.name} pauses DAGs at creation; Fabric's default is False"
    )


def test_the_e2e_sidecar_matches_the_shipped_one():
    """The e2e carries its own copy, so it can witness a topology nobody runs.

    That is not hypothetical: this pair drifted while the executor was being
    changed, leaving the e2e green against Sequential after the shipped sidecar
    had moved to Celery.
    """
    shipped, e2e = airflow_block(SHIPPED), airflow_block(E2E)
    keys = (
        "AIRFLOW__CORE__EXECUTOR",
        "AIRFLOW__CORE__DAGS_ARE_PAUSED_AT_CREATION",
        "AIRFLOW__CELERY__BROKER_URL",
    )
    for key in keys:
        a = [ln.strip() for ln in shipped.splitlines() if key in ln]
        b = [ln.strip() for ln in e2e.splitlines() if key in ln]
        assert a == b, f"{key} differs: shipped={a} e2e={b}"
