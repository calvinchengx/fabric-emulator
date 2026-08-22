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

A TEMP VIEW SHADOWS ONLY THE UNQUALIFIED NAME, which is why securing a session
means editing its catalog as well as adding a view. `catalog.register()` puts
every table in `default` too, so unqualified names resolve the way they do in a
lakehouse-attached notebook -- and that convenience registration was a door
straight past the filter. Measured (e2e/onelake-security-bypass): with the view
in place, `SELECT count(*) FROM sales` returned 2 rows of 3 and one column of
two, while `SELECT count(*) FROM default.sales` returned all 3 rows and both
columns. So every QUALIFIED registration of a secured table is removed from the
session, leaving the view as the only way to name it.

WHICH IS ONLY SOUND ON A PRIVATE CATALOG. Removing a registration is a change to
whatever catalog the session has, so on a SHARED one it would take the table
away from everybody -- enforcement for one user as an outage for the rest. This
refuses instead. Measured for the Connect route (Sail gives each session its
own catalog, and the owner was untouched); assumed for `newSession()`, whose
JVM contract shares the catalog by design.

SCOPE, STATED. Spark SQL and the catalog-resolved DataFrame readers.
`spark.read.format("delta").load(...)` bypasses the catalog entirely and is a
documented gap, not a claim: doc 54 records it and the parity row says so. Real
Fabric answers that one at the platform layer -- OneLake blocks direct path
reads of a secured table for non-privileged users -- not in the engine.
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


class CannotEnforce(RuntimeError):
    """This session cannot be secured, so no statement may run against it.

    Raised rather than returned. A caller that forgets to check a return value
    gets unfiltered data; one that forgets to catch an exception gets an error,
    and only one of those two mistakes is a leak.
    """


def schemas_holding(spark, name):
    """Every schema in this session that has `name` registered as a TABLE.

    Enumerated rather than assumed. `catalog.register()` puts a table in its
    lakehouse schema AND in `default`, `_make_unqualified_resolve` can add more,
    and a notebook is free to register its own -- so the set of doors is a fact
    about the session, not something this module can predict.

    Temporary views are excluded: the filter we install is one, and sweeping it
    away as a "qualified registration" would delete the enforcement.
    """
    found = []
    try:
        dbs = [r[0] for r in spark.sql("SHOW DATABASES").collect()]
    except Exception:  # noqa: BLE001 - an engine with no schemas has no qualified names
        return found
    for db in dbs:
        try:
            rows = spark.sql(f"SHOW TABLES IN `{db}`").collect()
        except Exception:  # noqa: BLE001 - a schema we cannot list holds nothing we can drop
            continue
        for row in rows:
            # (namespace, tableName, isTemporary) is the Spark shape; a shorter
            # row from another engine is treated as non-temporary.
            if row[1] != name:
                continue
            if len(row) > 2 and row[2]:
                continue
            found.append(db)
    return found


def restore(spark, name, location_of, log=None):
    """Put the real table back before re-securing it, returning whether it did.

    WHY THIS EXISTS. `apply()` runs per statement, and the sweep removes the
    table it built the view from. On the SECOND statement the filter's own SQL
    -- author-written text like `SELECT * FROM sales WHERE region_id = 1` --
    resolves to the view rather than the table. Applying a filter to an
    already-filtered relation happens to give the same rows, but a CLS
    projection has by then removed the columns a row filter may name, so the
    rebuild fails and the table is withheld: a session that worked once and
    stopped working, for no reason the user can see.

    Re-registering from the recorded LOCATION makes each application start from
    the same place as the first, which also means a policy edited mid-session
    takes effect. Without a location there is nothing to restore from, and the
    caller keeps the view it has.
    """
    location = location_of(name) if location_of else None
    if not location:
        # Silence here is how the first cut of this failed in the livy e2e: the
        # table was not restored, the rebuild read a name that no longer
        # resolved, and the only trace was "withheld". Say which of the two
        # happened.
        if log:
            log(f"onelake-security: {name} has no recorded location, "
                "so the installed view is kept as-is")
        return False
    try:
        spark.sql(f"DROP VIEW IF EXISTS {name}")
        # UNQUALIFIED, so it lands in the CURRENT database — which is the one
        # the filter's own unqualified SQL resolves against. Re-registering into
        # `default` looks equivalent and is not: the agent sets the current
        # database to the lakehouse schema (`lake`), so a table put back in
        # `default` is invisible to `SELECT ... FROM sales`.
        spark.sql(f"CREATE TABLE IF NOT EXISTS `{name}` "
                  f"USING delta LOCATION '{location}'")
        return True
    except Exception as exc:  # noqa: BLE001
        # Failing to restore is not failing to secure: the view is gone, so the
        # name resolves to nothing until the next attempt rebuilds it. Loud,
        # and closed.
        if log:
            log(f"onelake-security: {name} could not be restored ({exc})")
        return False


def apply(spark, access, tables, log=None, catalog_private=True, location_of=None):
    """Reshape the session so user SQL sees only permitted data.

    `access` is fetch_access's map; `tables` is every table currently visible.
    Unrestricted grants are left alone -- replacing a table with `SELECT * FROM
    table` would be a no-op that could only introduce bugs.

    `catalog_private` says whether this session's catalog is its own. When it is
    not, this refuses: see the module docstring.

    `location_of(name)` returns where a table's data lives, or None. Injected
    rather than imported so this module stays a pure function of its inputs and
    testable without an engine; the agent passes `delta_ops.known_location`.
    """
    unrestricted = access.get("*")
    secured = [n for n in tables
               if access.get(n, unrestricted) is None
               or access.get(n, unrestricted)["rows"]
               or access.get(n, unrestricted)["columns"]]
    if secured and not catalog_private:
        raise CannotEnforce(
            "this Spark session shares its catalog with others, so OneLake "
            f"security cannot be applied to {', '.join(sorted(secured))} "
            "without removing those tables from everyone. Refusing rather than "
            "serving unfiltered rows."
        )

    for name in tables:
        entry = access.get(name, unrestricted)
        if entry is None:
            _drop(spark, name, log)
            continue
        if not entry["rows"] and not entry["columns"]:
            continue  # permitted in full: leave the real table in place
        sql = secure_view_sql(name, entry)
        # Re-securing an already-secured session: start from the table, not
        # from last statement's view. See restore().
        if not schemas_holding(spark, name):
            restore(spark, name, location_of, log)
        try:
            # ORDER IS LOAD-BEARING. The view is built from the real table, so
            # it must exist while the view is created and be gone before the
            # statement runs. Sweeping first would leave nothing to select from.
            spark.sql(f"CREATE OR REPLACE TEMP VIEW {name} AS {sql}")
            holders = _sweep_qualified(spark, name)
            if log:
                where = f", qualified names dropped from {holders}" if holders else ""
                log(f"onelake-security: {name} narrowed{where}")
        except Exception as exc:  # noqa: BLE001
            # A view we cannot build must not leave the unfiltered table
            # readable: fail closed by removing it.
            _drop(spark, name, log)
            if log:
                log(f"onelake-security: {name} withheld ({exc})")


def _sweep_qualified(spark, name):
    """Remove every qualified registration of `name`, returning the schemas hit."""
    hit = []
    for db in schemas_holding(spark, name):
        try:
            spark.sql(f"DROP TABLE IF EXISTS `{db}`.`{name}`")
            hit.append(db)
        except Exception:  # noqa: BLE001 - report what was dropped, not what was tried
            pass
    return hit


def _drop(spark, name, log=None):
    # The qualified sweep first: a denied table must lose every spelling, and
    # `DROP TABLE <name>` alone only reaches the current database.
    _sweep_qualified(spark, name)
    for stmt in (f"DROP VIEW IF EXISTS {name}", f"DROP TABLE IF EXISTS {name}"):
        try:
            spark.sql(stmt)
        except Exception:  # noqa: BLE001 - whichever kind it was, it is gone now
            pass
    if log:
        log(f"onelake-security: {name} withheld")
