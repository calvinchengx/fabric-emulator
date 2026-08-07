"""CTAS with an explicit LOCATION — the shape dbt-fabricspark actually emits.

The ᵍ row of docs/engine-matrix.md records what this cost. dbt-fabricspark's
`file_format_clause` macro emits NO `USING` clause for exactly one value of
`file_format`: `delta`, the one the adapter assumes is the default. So a model
configured `+file_format: delta` with `+location_root` pointing at the lakehouse
emitted

    create or replace table silver.users location 'az://.../Tables/users'
    as select ...

which the CTAS grammar did not match — it had no LOCATION clause — so the
statement fell through to the engine, Sail wrote it into its own warehouse, and
**the lakehouse stayed empty while dbt reported success**. Two rounds of
debugging went into a config that was correct the whole time.

An explicit LOCATION is self-describing: the statement names its destination, so
it needs no schema registration to be honoured. That is what these pin.
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import delta_ops

DBT_SHAPED = (
    "create or replace table silver.users "
    "location 'az://ws/item/Tables/users' "
    "as select id, name from tmp_src"
)


def setup_function():
    delta_ops.forget_all()


def test_the_dbt_shaped_statement_is_intercepted_without_any_registration():
    # No remember_schema() call: an explicit LOCATION is enough on its own.
    kind, params = delta_ops.match(DBT_SHAPED)
    assert kind == "ctas"
    assert params["location"] == "az://ws/item/Tables/users"
    assert params["target"] == "silver.users"
    assert params["replace"]


def test_a_bare_name_with_a_location_is_intercepted_too():
    # `location_root` does not imply a schema-qualified name.
    kind, params = delta_ops.match(
        "CREATE TABLE users LOCATION '/lake/Tables/users' AS SELECT 1 AS a")
    assert kind == "ctas"
    assert params["location"] == "/lake/Tables/users"
    assert params["target"] == "users"


def test_using_delta_with_a_location_still_matches():
    kind, params = delta_ops.match(
        "CREATE TABLE t USING delta LOCATION '/lake/t' AS SELECT 1 AS a")
    assert kind == "ctas"
    assert params["using"].lower() == "delta"
    assert params["location"] == "/lake/t"


def test_a_non_delta_format_falls_through_to_the_engine():
    # `USING parquet` means parquet. Quietly writing Delta instead would be the
    # silent wrong thing this seam exists to prevent — worse than not helping.
    assert delta_ops.match(
        "CREATE TABLE t USING parquet LOCATION '/lake/t' AS SELECT 1 AS a") is None
    assert delta_ops.match(
        "CREATE TABLE t USING csv AS SELECT 1 AS a") is None


def test_without_a_location_the_schema_registry_still_decides():
    # The pre-existing rule is unchanged: no LOCATION and no registered schema
    # means the engine's warehouse is the right place, so this passes through.
    assert delta_ops.match("CREATE TABLE silver.users AS SELECT 1 AS a") is None

    delta_ops.remember_schema("silver", "az://ws/item/Tables/silver")
    kind, params = delta_ops.match("CREATE TABLE silver.users AS SELECT 1 AS a")
    assert kind == "ctas"
    assert not params["location"]


def test_an_explicit_location_wins_over_the_schema_default():
    # Both are available here. Overriding what the author WROTE with a schema
    # default would ignore the statement.
    delta_ops.remember_schema("silver", "az://ws/item/Tables/silver")
    _, params = delta_ops.match(DBT_SHAPED)
    assert params["location"] == "az://ws/item/Tables/users"


class FakeDF:
    def __init__(self, sink):
        self._sink = sink
        self.write = self

    def format(self, fmt):
        self._sink["format"] = fmt
        return self

    def mode(self, m):
        self._sink["mode"] = m
        return self

    def save(self, path):
        self._sink["path"] = path


def test_execute_writes_delta_to_the_stated_location():
    sink, statements = {}, []

    def original_sql(query):
        statements.append(query)
        return FakeDF(sink)

    _, params = delta_ops.match(DBT_SHAPED)
    message = delta_ops.execute_ctas(None, original_sql, params)

    # The rows land where the statement said, as Delta.
    assert sink["format"] == "delta"
    assert sink["path"] == "az://ws/item/Tables/users"
    assert sink["mode"] == "overwrite"  # `or replace`
    # And the name is registered afterwards, so later statements resolve it.
    assert any("CREATE TABLE IF NOT EXISTS" in s and "USING delta" in s for s in statements)
    assert delta_ops.known_location("silver.users") == "az://ws/item/Tables/users"
    assert "stated LOCATION" in message
