"""The same silver models, built by dbt WITHOUT the Livy hop.

    dbt-spark (method: session) -> Spark Connect -> Sail

`silver.py` builds these models through `dbt-fabricspark`, which speaks only
Livy: the adapter's one connection method is `FabricSparkConnectionMethod.LIVY`
and there is no `session` in it. That is the path a Fabric *pipeline* takes. A
Fabric *notebook* runs `dbtRunner` on the driver instead, and this is that
shape -- one transport removed, everything else identical: the same
`silver_dbt/` project, the same models, the same engine.

WHY IT EXISTS. silver.py records a `_rn`-not-in-schema failure it cannot
attribute: a probe ran the same SQL against Sail over Spark Connect and every
form passed, so the fault is "somewhere on the Livy path (emulator -> agent ->
Sail) or in dbt's generated SQL" and the two-CTE rewrite is kept without
knowing which. Running the models over a second transport is how that stops
being a guess. It narrows rather than settles it -- dbt-spark generates
different SQL from dbt-fabricspark, so a pass here rules out Sail and leaves
the adapter and the Livy path still to separate.

WHAT THE LIVY SURFACE WAS DOING FOR US, made visible by not having it. Opening
a Livy session registers the lakehouse's `Tables/` in the Spark catalog, the
way Fabric's metastore does -- which is why `silver_dbt/models/sources.yml`
resolves `bronze_orders` by name with no path anywhere. A bare Spark Connect
session has no such thing: `show databases` returns only `default`, and `lake`
does not exist. So this step registers what it reads, by OneLake location, in
the same `CREATE TABLE ... LOCATION` form the emulator's own session bootstrap
uses (python/spark_agent/catalog.py). That difference IS the finding: the Livy
surface is not merely a transport, it is also the metastore.

It writes into its own schema so the Livy-built silver is left standing and the
two can be compared rather than one overwriting the other.

    uv run python silver_session.py     # after `silver` has run
"""
import json
import os
import pathlib

HERE = pathlib.Path(__file__).resolve().parent
PROJECT = HERE / "silver_dbt"
STATE = json.loads((HERE / "state.json").read_text(encoding="utf-8"))

WORKSPACE = STATE.get("workspace_name") or STATE["workspace"]
LAKEHOUSE = STATE["lakehouse_name"]
SCHEMA = os.environ.get("SESSION_SCHEMA", f"{LAKEHOUSE}_session")
REMOTE = os.environ.get("SPARK_REMOTE", "sc://localhost:50051")
SOURCES = ("bronze_customers", "bronze_orders")


def onelake(table):
    """The location the emulator would have registered for this table."""
    return (f"abfss://{WORKSPACE}@onelake.dfs.fabric.microsoft.com/"
            f"{LAKEHOUSE}.Lakehouse/Tables/{table}")


def register():
    """Do by hand what opening a Livy session does for free."""
    os.environ["SPARK_REMOTE"] = REMOTE
    from pyspark.sql import SparkSession

    spark = SparkSession.builder.getOrCreate()
    for schema in (LAKEHOUSE, SCHEMA):
        spark.sql(f"CREATE SCHEMA IF NOT EXISTS `{schema}`")
    for table in SOURCES:
        spark.sql(f"CREATE TABLE IF NOT EXISTS `{LAKEHOUSE}`.`{table}` "
                  f"USING delta LOCATION '{onelake(table)}'")
    counts = {t: spark.sql(f"SELECT count(*) FROM `{LAKEHOUSE}`.`{t}`").collect()[0][0]
              for t in SOURCES}
    print(f"==> registered {len(SOURCES)} bronze table(s) by location: {counts}")
    return spark


def main():
    spark = register()
    (PROJECT / "profiles.yml").write_text(f"""contoso_silver_spark:
  target: dev
  outputs:
    dev:
      type: spark
      method: session
      host: localhost
      schema: "{SCHEMA}"
      threads: 4
""", encoding="utf-8")
    # A DISTINCT location, deliberately. silver.py lands these models in the
    # lakehouse's Tables/ area, which is where the SQL endpoint reflects from;
    # writing there too would have this step overwrite the very silver it exists
    # to be compared against. Files/ is a scratch area for that reason.
    onelake = (f"abfs://{STATE['workspace']}@onelake.dfs.fabric.microsoft.com"
               f"/{STATE['lakehouse']}/Files/silver_session")
    env = {**os.environ, "SPARK_REMOTE": REMOTE, "LAKEHOUSE_NAME": LAKEHOUSE,
           "ONELAKE_TABLES": onelake, "DBT_PROFILES_DIR": str(PROJECT)}
    # IN PROCESS, and that is not a convenience. Sail's catalog is SESSION
    # scoped: the tables registered above are invisible to any other session,
    # which is the same reason a bare Connect session cannot see what a Livy
    # session registered. Running dbt in a subprocess gives it its own
    # SparkSession and an empty catalog -- measured, `Table or view not found:
    # bronze_orders`. dbtRunner shares this process, so getOrCreate() hands dbt
    # the session that already has the sources in it.
    #
    # It is also the shape being demonstrated: a Fabric notebook runs dbtRunner
    # on the driver, which is the whole point of the session method.
    os.environ.update({k: v for k, v in env.items() if k not in os.environ or True})
    from dbt.cli.main import dbtRunner

    res = dbtRunner().invoke(["build", "--project-dir", str(PROJECT)])
    if not res.success:
        raise SystemExit(f"dbt build failed over the session method: {res.exception}")
    built = {}
    for model in ("silver_customers", "silver_orders", "silver_quarantine_orders"):
        built[model] = spark.sql(f"SELECT count(*) FROM `{SCHEMA}`.`{model}`").collect()[0][0]
    print(f"==> silver (dbt-spark, method: session) into `{SCHEMA}`: {built}")
    (HERE / "silver_session_summary.json").write_text(
        json.dumps({"schema": SCHEMA, "transport": "spark-connect", "rows": built},
                   indent=2), encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
