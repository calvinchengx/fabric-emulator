#!/usr/bin/env python3
"""e2e: the medallion tutorial, executed.

Brings up the emulator stack in docker, then runs
`examples/medallion-pyspark/pipeline.py` **on this machine**, exactly as the
example's README tells a reader to. The stack is containerised; the example is
not.

Containerising the client is how this harness ended up running over plain HTTP
against compose service names while the README documented self-signed TLS
against localhost — CI proving a path the documentation never describes. Running
the example the way a reader runs it removes that gap rather than documenting it
as an adaptation.

  python3 e2e/medallion/run.py

Needs the Microsoft ODBC Driver 18 on the host, because dbt-fabric and pyodbc do
(macOS: `brew install msodbcsql18`; CI installs it on the runner). Linux weight
class regardless — the SQL Server container is amd64-only.
"""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))
EXAMPLE = os.path.join(REPO, "examples", "medallion-pyspark")
# spark-agent is in this list because it EXECUTES the notebook cells. Since the
# notebook activity became synchronous the emulator drives the run through the
# agent, so a failing cell's traceback lives there and nowhere else — the
# emulator only logs "notebook drive … start" and then reports
# NotebookExecutionFailed. Without the agent's tail, a CI failure says a cell
# failed and cannot say why.
SERVICES_TO_LOG = ["fabric-emulator", "spark-agent", "sqlserver",
                   "keyvault-emulator", "sail"]

# The endpoints a reader uses: the stack's published ports over its self-signed
# TLS. KV_INTERNAL_URL is the one that differs in kind — Fabric resolves an
# AzureKeyVaultReference server-side, so the vault URI it STORES must be
# reachable from the fabric container, not from here.
ENDPOINTS = {
    "ENTRA_URL": "https://localhost:8443",
    "KV_URL": "https://localhost:8444",
    "FABRIC_REST_URL": "https://localhost:9443",
    # TDS_SERVER is deliberately NOT set: unset means the example discovers the
    # SQL address from the item's own properties, which is the only form that can
    # work against real Fabric. Setting it here would leave that path unexercised
    # by every CI run while looking covered.
    "SPARK_REMOTE": "sc://localhost:50051",
    "KV_INTERNAL_URL": "https://keyvault-emulator:8444",
}


# Coverage: layer the overlay so this suite's run contributes counters to
# the merged profile. Only when asked, so the default path is unchanged
# (e2e/docker-compose.coverage.yml, docs/10-testing.md).
def _cov():
    if not os.environ.get("FABRIC_COVERAGE"):
        return []
    return ["-f", "docker-compose.yml",
            "-f", os.path.join("..", "docker-compose.coverage.yml")]


def compose(*args, profiles=()):
    """`--profile` before the subcommand: compose only applies it there."""
    flags = [f for p in profiles for f in ("--profile", p)]
    return subprocess.run(["docker", "compose", *_cov(), *flags, *args], cwd=DIR).returncode


def run(example=EXAMPLE, label="medallion", profiles=()):
    """`profiles` names optional services this example needs.

    EVERY leg now needs the Livy agent, because Fabric's notebook activity is
    synchronous and the emulator drives the run through that agent. It used to
    be profiled away from the PySpark legs on the grounds that starting it cost
    them "a fourfold slowdown in lakehouse reflection" (ece5e17). Measured on CI
    when the legs were switched over: 5m -> 5m and 9m -> 10m. The fourfold cost
    was the `_rn` projection bug fixed in that same commit, not the agent."""
    try:
        # --wait blocks until every healthcheck passes, so the example never
        # races a backend that is still booting.
        rc = compose("up", "-d", "--build", "--wait", profiles=profiles)
        if rc == 0:
            rc = subprocess.run(
                ["uv", "run", "--project", example, "python", "pipeline.py"],
                cwd=example, env={**os.environ, **ENDPOINTS}).returncode
        if rc == 0 and label == "medallion":
            # The type map along the ROUTE, once a lakehouse exists.
            #
            # internal/warehouse proves the mapping in Go: a Parquet column of
            # each logical type reflects to the right SQL type. It cannot prove
            # the route, because reflection exists for Delta written by
            # SOMETHING ELSE and read by a real ODBC client, and neither end of
            # that seam is Go. This suite is the only one with both halves, so
            # the probe runs here rather than in a compose file of its own.
            #
            # Run inside the example's project so it borrows that example's
            # state and token helpers — the same route the tutorial documents.
            # The probe gets TDS_SERVER explicitly, and the EXAMPLE does not. It
            # is harness tooling rather than tutorial code: it dials the SQL
            # endpoint to check a type mapping, with no state.json of its own to
            # discover an item from. The example must discover its address (the
            # only form that works on real Fabric); the probe would gain nothing
            # from it and would only make this suite unable to prove the
            # discovery path is exercised.
            rc = subprocess.run(
                ["uv", "run", "--project", example, "python",
                 os.path.join(REPO, "e2e", "type-map", "probe.py")],
                cwd=example,
                env={**os.environ, **ENDPOINTS, "TDS_SERVER": "localhost,1433"}).returncode
            if rc != 0:
                print("\n==== type-map probe FAILED ====", file=sys.stderr)
        if rc != 0:
            print(f"\n==== {label} FAILED (exit {rc}) ====", file=sys.stderr)
            for svc in SERVICES_TO_LOG:
                print(f"\n==== {svc} logs (tail) ====", file=sys.stderr)
                compose("logs", "--tail", "80", svc, profiles=profiles)
        return rc
    finally:
        compose("down", "-v", "--remove-orphans", profiles=profiles)


if __name__ == "__main__":
    # The notebook activity is synchronous, so the emulator needs the Spark
    # agent to execute the cells — see run()'s docstring for the cost.
    sys.exit(run(profiles=("livy",)))
