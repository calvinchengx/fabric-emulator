"""The bounded MERGE grammar: what routes to delta-rs, and what falls through.

Parsing only — execution needs live storage and is covered by the e2e that
motivated the feature (Rosetta silver upsert against a temporal-columned
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


def test_unbounded_shapes_fall_through_to_the_engine():
    # subquery source, DELETE clause, or no recognisable branch: not ours.
    assert delta_ops.match(
        "MERGE INTO tgt t USING (SELECT 1 k) s ON t.k = s.k "
        "WHEN MATCHED THEN UPDATE SET t.v = s.v") is None
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
