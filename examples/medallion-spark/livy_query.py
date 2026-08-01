"""Run a Spark SQL statement over the emulator's Livy surface.

06_gold_spark.py reads its results back through the same transport dbt used,
rather than through delta-rs on the client. That is deliberate: if the star were
verified by a second reader, a bug in the Livy path could pass unnoticed, and
the Livy path is exactly what this example exists to exercise.

The wire is Fabric's own:

    POST …/livyapi/versions/{ver}/sessions            create a session
    POST …/livyapi/versions/{ver}/sessions/{id}/statements    submit
    GET  …/sessions/{id}/statements/{sid}             poll to available
"""
import pathlib as _pathlib
import sys as _sys

# The shared halves of this pipeline — endpoints, tokens, state, and the seeded
# fixture — live in the Warehouse example, because both paths ingest the same
# data from the same source system. Importing them beats copying a 700-line
# generator that would then have to be kept identical by hand.
_sys.path.insert(0, str(_pathlib.Path(__file__).resolve().parent.parent / "medallion"))

import time

from common import FABRIC, FABRIC_AUD, S, load, token

VER = "2023-12-01"
_session = None


def _base():
    st = load()
    return (f"{FABRIC}/v1/workspaces/{st['workspace']}"
            f"/lakehouses/{st['lakehouse']}/livyapi/versions/{VER}")


def _headers():
    return {"Authorization": "Bearer " + token(FABRIC_AUD)}


def session():
    """One session, reused: creating one per statement would measure session
    setup rather than the query."""
    global _session
    if _session is not None:
        return _session
    r = S.post(f"{_base()}/sessions", headers=_headers(), json={"kind": "sql"})
    assert r.status_code in (200, 201), f"livy session: {r.status_code} {r.text}"
    _session = r.json()["id"]
    for _ in range(120):
        s = S.get(f"{_base()}/sessions/{_session}", headers=_headers()).json()
        if s.get("state") in ("idle", "available"):
            break
        assert s.get("state") not in ("dead", "error", "killed"), s
        time.sleep(1)
    return _session


def query(sql, timeout=300):
    """Submit `sql` and return its rows as a list of lists."""
    sid = session()
    r = S.post(f"{_base()}/sessions/{sid}/statements", headers=_headers(),
               json={"code": sql, "kind": "sql"})
    assert r.status_code in (200, 201), f"livy statement: {r.status_code} {r.text}"
    stid = r.json()["id"]

    deadline = time.time() + timeout
    while time.time() < deadline:
        st = S.get(f"{_base()}/sessions/{sid}/statements/{stid}",
                   headers=_headers()).json()
        if st.get("state") == "available":
            out = st.get("output") or {}
            assert out.get("status") != "error", out
            payload = (out.get("data") or {}).get("application/json") or {}
            return payload.get("data", [])
        time.sleep(0.5)
    raise TimeoutError(f"statement {stid} did not complete in {timeout}s")
