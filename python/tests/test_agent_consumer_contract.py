"""The SQL shapes downstream consumers emit, asserted against the shared agent.

`emulator-spark-agent` is built and published HERE and consumed THERE:
databricks-emulator pulls the same image by digest, and its Warehouse SQL path
reaches `delta_ops` through statements this repo's own witnesses never send.
Nothing in fabric-emulator's CI stood between a change to that module and a
break in another repo — the image is published on tag, and the consumer finds
out on upgrade.

It has already cost one: `_CREATE_DELTA_LOCATION` required `USING` to sit
directly against the table name, which is what dbt-fabricspark emits, so every
witness here was green. databricks-emulator emits

    CREATE TABLE events (id INT, name STRING) USING delta LOCATION '…'

which did not match, so the location went unrecorded, and the MERGE two
statements later fell through to `resolve()`'s DESCRIBE DETAIL and died in
Sail's parser. Four agent releases shipped with it.

**This file is the contract.** Every row is a statement shape a named consumer
actually sends, cited to the file it comes from, with the answer the agent owes
it. A consumer adds a row when it starts emitting a new shape; a change here
that breaks one fails in the repo that would ship the regression, not in the
repo that would suffer it.

What it does NOT do is prove the statement *executes* — that needs an engine and
belongs to each repo's witnesses. It proves recognition, which is the half that
silently degrades: an unmatched shape raises nothing at the point of the miss.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "spark_agent"))

import delta_ops as d  # noqa: E402

# (consumer, source, sql, expected, given)
#
# expected is either ("match", kind) — `match()` must claim it and route it to
# that handler — or ("remember", table, location): the statement is the engine's
# to execute, but the agent must record where the table lives, because a later
# OPTIMIZE / MERGE / DESCRIBE of that name has no other way to find it.
#
# `given` is the schema registry the statement arrives with, schema -> location.
# It is not scaffolding: CTAS interception is *conditional* on it, because a
# schema nobody gave a location for belongs in the engine's own warehouse. A row
# that needs it and does not say so is asserting the wrong thing.
CONTRACT = [
    (
        "databricks-emulator",
        "e2e/delta/run.py — Warehouse SQL, the shape `databricks-sdk` sends",
        "CREATE TABLE events (id INT, name STRING) USING delta "
        "LOCATION '/data/e2e/events'",
        ("remember", "events", "/data/e2e/events"),
        {},
    ),
    (
        "databricks-emulator",
        "e2e/delta/run.py — upsert against the table created above",
        "MERGE INTO events AS t "
        "USING (SELECT * FROM VALUES (3, 'carol-upd'), (4, 'dave') AS s(id, name)) AS s "
        "ON t.id = s.id "
        "WHEN MATCHED THEN UPDATE SET t.name = s.name "
        "WHEN NOT MATCHED THEN INSERT *",
        ("match", "merge"),
        {},
    ),
    (
        "databricks-emulator",
        "internal/sqlshim — managed CREATE, rewritten to carry a LOCATION",
        "CREATE TABLE IF NOT EXISTS main.default.t (id BIGINT) USING delta "
        "LOCATION '/data/__unitystorage/t'",
        ("remember", "main.default.t", "/data/__unitystorage/t"),
        {},
    ),
    (
        "fabric-emulator",
        "dbt-fabricspark with +location_root — the shape that has always worked",
        "CREATE TABLE silver.orders USING delta LOCATION 'abfss://lake/Tables/orders'",
        ("remember", "silver.orders", "abfss://lake/Tables/orders"),
        {},
    ),
    (
        "fabric-emulator",
        "dbt-fabricspark table materialization",
        "CREATE OR REPLACE TABLE gold.fct AS SELECT 1 AS n",
        ("match", "ctas"),
        {"gold": "abfss://lake/Tables/gold"},
    ),
    (
        "fabric-emulator",
        "notebook maintenance — docs/engine-matrix.md",
        "OPTIMIZE delta.`abfss://lake/Tables/orders`",
        ("match", "optimize"),
        {},
    ),
    (
        "fabric-emulator",
        "notebook maintenance — docs/engine-matrix.md",
        "VACUUM silver.orders RETAIN 168 HOURS",
        ("match", "vacuum"),
        {},
    ),
    # Column types carrying their own parentheses: the column list has to be
    # matched paren-aware, or `DECIMAL(10,2)` truncates it and the LOCATION is
    # lost exactly as it was above, without a symptom until much later.
    (
        "databricks-emulator",
        "Warehouse SQL — a decimal column, the case a naive `\\([^)]*\\)` loses",
        "CREATE TABLE priced (id INT, amount DECIMAL(10,2)) USING delta "
        "LOCATION '/data/priced'",
        ("remember", "priced", "/data/priced"),
        {},
    ),
    # Spark allows clauses between USING and LOCATION. dbt emits partitioning
    # this way, and LOCATION arriving after them must still be recorded.
    # A dbt model file opens with a comment, so the statement the adapter sends
    # carries one between AS and SELECT. Copied VERBATIM from a dbt-fabricspark
    # debug log -- whitespace, backquoted relation and all -- because every
    # hand-written probe of this shape passed and only the adapter's own output
    # failed. A tidied version of this row would have proved nothing.
    (
        "fabric-emulator",
        "dbt-fabricspark table materialization — contoso-airflow-data-product "
        "dbt/silver/models/silver_product_hierarchy.sql, via logs/dbt.log",
        "create or replace table `lake`.silver_product_hierarchy\n"
        "      \n      \n"
        "    location 'abfss://contoso-analytics@onelake.dfs.fabric.microsoft.com"
        "/lake.Lakehouse/Tables/silver_product_hierarchy'\n"
        "      \n\n      as\n      \n"
        "-- The reference vendor's hierarchy, as-is.\n"
        "--\n"
        "-- Deliberately thin: this vendor is the group data office's publisher.\n"
        "select * from `default`.bronze_ref_product_hierarchy",
        ("match", "ctas"),
        {},
    ),
    # A CTE, which is how dbt's own documentation writes models and how most
    # projects follow it. Requiring SELECT claimed the minority of a real
    # silver layer and missed the majority: measured on one project, the three
    # models opening with SELECT landed in OneLake with a _delta_log and the
    # five opening with WITH left bare parquet at the same paths -- unreadable
    # as a table, and reported as success by both halves.
    (
        "fabric-emulator",
        "dbt-fabricspark — contoso-airflow-data-product "
        "dbt/silver/models/silver_customers.sql, the canonical `with … select`",
        "create or replace table `lake`.silver_customers\n"
        "    location 'abfss://lake/Tables/silver_customers'\n"
        "      as\n"
        "-- THE DUPLICATES ARE DELIBERATE. The POS vendor ships a 2% ratio.\n"
        "with ranked as (\n"
        "    select *, row_number() over (partition by customer_id "
        "order by updated_at desc) as rn\n"
        "    from `default`.bronze_pos_customers\n"
        ")\n"
        "select * from ranked where rn = 1",
        ("match", "ctas"),
        {},
    ),
    # The same miss through the other comment syntax. dbt's own query_comment
    # is a block comment, and a fix that handles only `--` leaves this open.
    (
        "fabric-emulator",
        "dbt-fabricspark — a model whose header is a block comment "
        "(and the query_comment adapters prepend)",
        '/* {"app": "dbt", "node_id": "model.contoso_silver.x"} */\n'
        "create or replace table `lake`.x\n"
        "    location 'abfss://lake/Tables/x'\n"
        "      as\n"
        "/* what this model is for */\n"
        "select 1 as n",
        ("match", "ctas"),
        {},
    ),
    (
        "fabric-emulator",
        "dbt-fabricspark with +partition_by",
        "CREATE TABLE silver.evt (id INT, d DATE) USING delta PARTITIONED BY (d) "
        "LOCATION 'abfss://lake/Tables/evt'",
        ("remember", "silver.evt", "abfss://lake/Tables/evt"),
        {},
    ),
]


@pytest.fixture(autouse=True)
def _clean_registry():
    d.forget_all()
    yield
    d.forget_all()


@pytest.mark.parametrize(
    "consumer,source,sql,expected,given",
    CONTRACT,
    ids=[f"{c}:{s[:44]}" for c, _, s, _, _ in CONTRACT],
)
def test_the_agent_honours_the_shape_a_consumer_sends(consumer, source, sql,
                                                      expected, given):
    for schema, location in given.items():
        d.remember_schema(schema, location)
    kind = expected[0]
    if kind == "match":
        got = d.match(sql)
        assert got is not None, (
            f"{consumer} sends this and the agent no longer claims it "
            f"({source}). It would fall through to the engine.")
        assert got[0] == expected[1], (
            f"{consumer}: routed to {got[0]!r}, contract says {expected[1]!r} "
            f"({source})")
    else:
        _, table, location = expected
        # The engine executes it; the agent's job is to notice the path.
        assert d.match(sql) is None or d.match(sql)[0] == "ctas", (
            f"{consumer}: this is the engine's statement to run ({source})")
        d.remember_stated_delta_location(sql)
        assert d.known_location(table) == location, (
            f"{consumer} creates {table!r} this way and the agent did not "
            f"record its location ({source}). Nothing fails here — it fails at "
            f"the next MERGE/OPTIMIZE of that name, in resolve(), as a parse "
            f"error from an engine that has no DESCRIBE DETAIL.")


def test_every_contract_row_cites_the_consumer_file_it_came_from():
    # A row without provenance cannot be checked against the consumer when the
    # consumer changes, which is the only thing keeping this file honest.
    for consumer, source, sql, _, _given in CONTRACT:
        assert consumer and "-emulator" in consumer
        assert source and len(source) > 20, f"{consumer}: {sql[:40]!r} has no source"
