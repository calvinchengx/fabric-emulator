#!/usr/bin/env python3
"""Execute the consumer contract against the agent IMAGE, before it is published.

`python/tests/test_agent_consumer_contract.py` proves the agent *recognises* the
statement shapes downstream consumers send. It runs in milliseconds and needs no
engine, which is why it can guard every PR — and also why it cannot catch a
whole class of break. Recognition is not execution:

  - The `'3GB'` failure was not a shape at all. The agent recognised everything
    and died in the pyspark client on a conf value the engine served, which no
    amount of regex matching would surface.
  - A shape can match and still fail downstream of the match: a resolved
    location that is wrong, a delta-rs call that refuses, an interception that
    returns something the caller cannot use.

So this is the same CONTRACT, run for real: a live Sail, the agent built from
the Dockerfile `compute-images` publishes, and the statements sent over the
agent's own HTTP surface the way the emulator sends them.

WHY IT GATES THE PUBLISH. The bug that motivated this shipped in four agent
releases (0.25.0 through 0.26.0). Every one of them was green here, because
fabric-emulator's own witnesses address Delta BY PATH — `OPTIMIZE delta.\\`uri\\``
— which never calls `resolve()`, which is where the break was. databricks-emulator
addresses tables BY NAME, and found out on upgrade. A gate that runs the other
consumer's shapes before pushing is the thing that turns that discovery around.

WHAT IT DOES NOT PROVE: this builds linux/amd64 only. `compute-images` publishes
a multi-arch manifest, so an arm64-specific fault would still reach a consumer.
Sharpening that means running the gate under QEMU, which costs more than it is
worth until an arm64-only fault actually happens.
"""
import json
import pathlib
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parents[1]
AGENT = "http://127.0.0.1:18099"
TABLE_DIR = "/tmp/contract"

COMPOSE = ["docker", "compose", "-f", str(HERE / "docker-compose.yml"),
           "-p", "fe-agent-contract"]


def compose(*args, check=True):
    return subprocess.run(COMPOSE + list(args), check=check, cwd=ROOT)


def wait_health(timeout=300):
    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(AGENT + "/health", timeout=5) as r:
                if r.status == 200:
                    return
        except (urllib.error.URLError, OSError) as exc:  # noqa: PERF203
            last = str(exc)
        time.sleep(2)
    raise SystemExit(f"agent never came up at {AGENT}/health: {last}")


def stmt(code, kind="sql", session="contract"):
    """Send one statement the way the emulator's Livy path does."""
    body = json.dumps({"session": session, "kind": kind, "code": code}).encode()
    req = urllib.request.Request(AGENT + "/statements", data=body,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=300) as r:
        return json.loads(r.read())


def ok(code, why, kind="sql"):
    """Run a statement that must succeed, and say what it was FOR when it does not.

    The agent answers 200 with an error payload rather than an HTTP error, so a
    caller that only checks the status code reports every failure as a pass —
    which is the shape of bug this whole gate exists to prevent.
    """
    res = stmt(code, kind=kind)
    if res.get("status") != "ok":
        detail = json.dumps(res)[:600]
        raise SystemExit(f"CONTRACT BROKEN — {why}\n  statement: {code.strip()[:200]}\n  {detail}")
    return res


def text(res):
    """REPL output, for `pyspark` statements."""
    return (res.get("data") or {}).get("text/plain", "")


def rows_of(res):
    """Result rows, for `sql` statements.

    sqlrun answers SQL under `application/json`, not `text/plain` — reading the
    wrong key returns "" and every assertion on it passes vacuously, so this is
    a separate accessor rather than a default on the one above.
    """
    payload = (res.get("data") or {}).get("application/json") or {}
    return payload.get("data") or []


def main() -> int:
    compose("down", "-v", check=False)
    compose("build")
    compose("up", "-d")
    try:
        wait_health()

        # 1. The shape that broke. A column list between the name and USING is
        #    what databricks-emulator's Warehouse SQL emits, and what the agent
        #    silently failed to record — with no symptom until (3).
        events = f"{TABLE_DIR}/events"
        ok(f"CREATE TABLE events (id INT, name STRING) USING delta LOCATION '{events}'",
           "the agent must record a LOCATION from a CREATE carrying a column list")

        ok("INSERT INTO events VALUES (1, 'alice'), (2, 'bob')",
           "the engine must accept an ordinary insert into the named table")

        # 2. DESCRIBE DETAIL by NAME. Sail has no DETAIL in its grammar, so this
        #    only works if the agent intercepts it AND resolved the location
        #    from what it recorded at (1). This is the assertion that would have
        #    failed for four straight releases.
        detail = ok("DESCRIBE DETAIL events",
                    "DESCRIBE DETAIL on a NAMED table must be served by the agent, "
                    "not forwarded to an engine whose parser rejects DETAIL")
        got_detail = rows_of(detail)
        if not got_detail or "delta" not in json.dumps(got_detail).lower():
            raise SystemExit(f"DESCRIBE DETAIL answered without a format: {got_detail}")
        if events not in json.dumps(got_detail):
            raise SystemExit(
                f"DESCRIBE DETAIL resolved to the wrong location: {got_detail}\n"
                f"  expected it to name {events}")

        # 3. MERGE by NAME — the statement that failed as `found DETAIL at 9:15`.
        ok("""MERGE INTO events AS t
              USING (SELECT * FROM VALUES (2, 'bob-upd'), (3, 'carol') AS s(id, name)) AS s
              ON t.id = s.id
              WHEN MATCHED THEN UPDATE SET t.name = s.name
              WHEN NOT MATCHED THEN INSERT *""",
           "MERGE against a NAMED target must resolve through the recorded "
           "location; this is the exact statement that regressed in 0.25.0")

        # 4. OPTIMIZE by NAME. Same resolve() path, different caller — the one
        #    whose silent degradation to 'skipped' started all of this.
        ok("OPTIMIZE events",
           "OPTIMIZE on a NAMED table must reach delta-rs through the recorded location")

        # 5. Confirm with a reader that is NOT the writer. Sail wrote; delta-rs
        #    reads the log independently. A COUNT(*) from Sail would only prove
        #    Sail agrees with itself.
        rows = ok(f"""
import json
from deltalake import DeltaTable
dt = DeltaTable('{events}')
t = dt.to_pyarrow_table()
cols = {{c.lower(): c for c in t.column_names}}
got = sorted(zip(t.column(cols['id']).to_pylist(), t.column(cols['name']).to_pylist()))
print(json.dumps([[int(i), str(n)] for i, n in got]))
""", "delta-rs must read back what the MERGE wrote", kind="pyspark")
        got = json.loads(text(rows).strip().splitlines()[-1])
        want = [[1, "alice"], [2, "bob-upd"], [3, "carol"]]
        if got != want:
            raise SystemExit(f"delta-rs read {got}, contract expects {want}")

        # 6. createDataFrame — not a shape, an engine-conf assumption. This is
        #    the call that died with `invalid literal for int() with base 10:
        #    '3GB'` on an engine serving a string, and no shape test can see it.
        made = ok("print(spark.createDataFrame([(1, 'a')], ['n', 's']).count())",
                  "createDataFrame must survive whatever this engine serves for "
                  "localRelationSizeLimit; a client int() on it is the '3GB' break",
                  kind="pyspark")
        if "1" not in text(made):
            raise SystemExit(f"createDataFrame returned {text(made)[:200]}")

        print("\ne2e/agent-contract: consumer shapes execute against the built "
              f"agent image — named CREATE/DESCRIBE DETAIL/MERGE/OPTIMIZE, "
              f"delta-rs confirmed {want}, createDataFrame live", flush=True)
        return 0
    finally:
        compose("logs", "--no-color", "--tail", "80", "spark-agent", check=False)
        compose("down", "-v", check=False)


if __name__ == "__main__":
    sys.exit(main())
