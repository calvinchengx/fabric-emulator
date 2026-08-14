"""The bounded MERGE grammar: what routes to delta-rs, and what falls through.

Parsing only — execution needs live storage and is covered by the e2e that
motivated the feature (a silver-layer upsert against a temporal-columned
target, which Sail's own planner cannot resolve).
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import delta_ops

SILVER_SHAPED = """
MERGE INTO silver.la_cd2_users t
USING tmp_silver_abc123 s
ON t.key_hash = s.key_hash
WHEN MATCHED AND t.row_hash <> s.row_hash THEN
  UPDATE SET
    t.`name` = s.`name`,
    t.`Strm` = s.`Strm`
WHEN NOT MATCHED THEN
  INSERT (`name`, `Strm`, `key_hash`)
  VALUES (s.`name`, s.`Strm`, s.`key_hash`)
"""


def test_silver_shaped_merge_matches():
    kind, p = delta_ops.match(SILVER_SHAPED)
    assert kind == "merge"
    assert p["target"] == "silver.la_cd2_users"
    assert p["source"] == "tmp_silver_abc123"
    assert p["talias"] == "t" and p["salias"] == "s"
    assert p["on"].strip() == "t.key_hash = s.key_hash"
    assert "row_hash" in p["mcond"]
    assert "`Strm`" in p["sets"]
    assert "`key_hash`" in p["icols"]


def test_update_only_and_insert_only_match():
    up, _ = delta_ops.match(
        "MERGE INTO tgt t USING src s ON t.k = s.k "
        "WHEN MATCHED THEN UPDATE SET t.v = s.v")
    ins, _ = delta_ops.match(
        "MERGE INTO tgt t USING src s ON t.k = s.k "
        "WHEN NOT MATCHED THEN INSERT (k, v) VALUES (s.k, s.v)")
    assert up == "merge" and ins == "merge"


PROBE_SHAPED = """
MERGE INTO m_reg AS t
USING (SELECT * FROM VALUES (1, 'b') AS s(id, v)) AS s
ON t.id = s.id
WHEN MATCHED THEN UPDATE SET t.v = s.v
WHEN NOT MATCHED THEN INSERT *
"""

PATH_PROBE_SHAPED = """
MERGE INTO delta.`/tmp/probe/t_merge_path` AS t
USING (SELECT * FROM VALUES (1, 'b') AS s(id, v)) AS s
ON t.id = s.id
WHEN MATCHED THEN UPDATE SET t.v = s.v
WHEN NOT MATCHED THEN INSERT *
"""


def test_engine_matrix_probe_shape_matches():
    # The generated MERGE rows use this exact shape. Named-source INSERT (cols)
    # VALUES was already ours; these two holes are why the middle column stayed
    # red after the medallion matcher landed.
    kind, p = delta_ops.match(PROBE_SHAPED)
    assert kind == "merge"
    assert p["target"] == "m_reg"
    assert p["source"] is None
    assert "SELECT" in p["source_query"].upper()
    assert "VALUES" in p["source_query"].upper()
    assert p["istar"] == "*"
    assert p["icols"] is None
    assert p["talias"] == "t" and p["salias"] == "s"
    assert "t.v = s.v" in p["sets"]


def test_path_target_with_subquery_and_insert_star_matches():
    kind, p = delta_ops.match(PATH_PROBE_SHAPED)
    assert kind == "merge"
    assert p["target"] == "delta.`/tmp/probe/t_merge_path`"
    assert p["istar"] == "*"
    assert "SELECT" in p["source_query"].upper()


def test_insert_star_with_a_named_source_matches():
    kind, p = delta_ops.match(
        "MERGE INTO tgt t USING src s ON t.k = s.k "
        "WHEN NOT MATCHED THEN INSERT *")
    assert kind == "merge"
    assert p["source"] == "src"
    assert p["istar"] == "*"
    assert p["sets"] is None


def test_unbounded_shapes_fall_through_to_the_engine():
    # DELETE, or no recognisable branch: not ours. Subquery source is in scope.
    assert delta_ops.match(
        "MERGE INTO tgt t USING src s ON t.k = s.k "
        "WHEN MATCHED THEN DELETE") is None
    assert delta_ops.match("SELECT 1") is None


def test_dequote_and_split_helpers():
    assert delta_ops._dequote("t.`Strm` = s.`Strm`") == 't."Strm" = s."Strm"'
    parts = delta_ops._split_top("t.a = concat_ws('||', s.b, s.c), t.d = s.e")
    assert parts == ["t.a = concat_ws('||', s.b, s.c)", "t.d = s.e"]
    assert delta_ops._plain_column("t.`Src_Snpsht_Dt`", "t") == "Src_Snpsht_Dt"


def test_ctas_matches_only_registered_schemas():
    delta_ops.forget_all()
    sql = "CREATE OR REPLACE TABLE gold.stg_x AS SELECT * FROM gold.stg_y WHERE 1 = 0"
    # unknown schema: the engine's own warehouse is the right place
    assert delta_ops.match(sql) is None
    delta_ops.remember_schema("gold", "abfss://ws@host/lh/Tables/gold/")
    kind, p = delta_ops.match(sql)
    assert kind == "ctas"
    assert p["target"] == "gold.stg_x"
    assert p["replace"]
    assert p["query"].startswith("SELECT * FROM gold.stg_y")
    # one-part names and non-SELECT bodies fall through
    assert delta_ops.match("CREATE TABLE bare AS SELECT 1") is None
    assert delta_ops.match("CREATE TABLE gold.t (x INT)") is None
    delta_ops.forget_all()
