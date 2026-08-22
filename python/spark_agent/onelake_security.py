"""Row and column security, applied by the engine — stage 5 of doc 54.

WHY THE ENGINE AND NOT ONELAKE. OneLake decides policy and hands back row
filters as SQL TEXT; whoever runs the query applies them. Fabric's own Spark and
SQL endpoints do exactly this, and the `principalAccess` API exists so a
third-party engine can too. This module is the agent playing that role for
notebook SQL.

HOW IT APPLIES, AND WHY NOT BY REWRITING QUERIES. Rewriting arbitrary user SQL
would mean parsing it, and a parser that is 95% right is a security control that
is 5% absent. Instead the SECURED RELATION is replaced: a table the user may see
with a filter becomes a temp view holding the filter, and a table they may not
see is removed from the session. User SQL is then untouched -- `SELECT * FROM
sales` reads the view, and the filter cannot be escaped by writing the query
differently.

SCOPE, STATED. Spark SQL only. `spark.read.format("delta").load(...)` bypasses
the catalog entirely and is a documented gap, not a claim: doc 54 records it and
the parity row says so. Covering it needs its own interception and its own
witness.
"""
import json
import urllib.request


def fetch_access(base_url, workspace, item, principal, token, opener=None):
    """Effective access for one principal, from the OneLake security API.

    Returns {table_name: entry}. A table absent from the map is one the
    principal may not read at all -- the deny-by-default answer, which is a
    result rather than an error.
    """
    url = (f"{base_url}/onelake/v1.0/workspaces/{workspace}"
           f"/artifacts/{item}/securityPolicy/principalAccess")
    body = json.dumps({"aadObjectId": principal, "inputPath": "Tables"}).encode()
    req = urllib.request.Request(url, data=body, method="GET",
                                 headers={"Content-Type": "application/json",
                                          "Authorization": "Bearer " + token})
    do = opener or urllib.request.urlopen
    with do(req) as r:
        payload = json.loads(r.read() or b"{}")
    out = {}
    for e in payload.get("value") or []:
        path = (e.get("path") or "").strip("/")
        if not path.lower().startswith("tables"):
            continue
        name = path[len("Tables"):].strip("/")
        # `Tables` alone means the whole half: an unrestricted grant, which the
        # caller reads as "every table, unfiltered".
        out["*" if name == "" else name.split("/")[-1]] = {
            "rows": e.get("rows") or "",
            "columns": list(e.get("columns") or []),
        }
    return out


def secure_view_sql(table, entry):
    """The SELECT that becomes the user's view of `table`.

    Columns narrow the projection, rows narrow the relation, and both compose:
    the row filter is the author's own SQL, so it is used as the source rather
    than pasted into a WHERE we would have to parse.
    """
    cols = ", ".join(entry["columns"]) if entry["columns"] else "*"
    src = f"({entry['rows']})" if entry["rows"] else table
    return f"SELECT {cols} FROM {src}"


def apply(spark, access, tables, log=None):
    """Reshape the session so user SQL sees only permitted data.

    `access` is fetch_access's map; `tables` is every table currently visible.
    Unrestricted grants are left alone -- replacing a table with `SELECT * FROM
    table` would be a no-op that could only introduce bugs.
    """
    unrestricted = access.get("*")
    for name in tables:
        entry = access.get(name, unrestricted)
        if entry is None:
            _drop(spark, name, log)
            continue
        if not entry["rows"] and not entry["columns"]:
            continue  # permitted in full: leave the real table in place
        sql = secure_view_sql(name, entry)
        try:
            spark.sql(f"CREATE OR REPLACE TEMP VIEW {name} AS {sql}")
            if log:
                log(f"onelake-security: {name} narrowed")
        except Exception as exc:  # noqa: BLE001
            # A view we cannot build must not leave the unfiltered table
            # readable: fail closed by removing it.
            _drop(spark, name, log)
            if log:
                log(f"onelake-security: {name} withheld ({exc})")


def _drop(spark, name, log=None):
    for stmt in (f"DROP VIEW IF EXISTS {name}", f"DROP TABLE IF EXISTS {name}"):
        try:
            spark.sql(stmt)
        except Exception:  # noqa: BLE001 - whichever kind it was, it is gone now
            pass
    if log:
        log(f"onelake-security: {name} withheld")
