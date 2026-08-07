"""delta_ops decides WHICH ENGINE runs a statement — and it does so by regex.

The module intercepts `spark.sql`: a statement it matches goes to delta-rs, a
statement it does not goes to the engine untouched. Both directions are
dangerous in a way coverage alone will not show:

  * matching too much sends a statement delta-rs cannot faithfully run;
  * matching too little silently returns to Sail, which for CTAS writes to its
    own warehouse and leaves the lakehouse EMPTY — the false-green write this
    whole module exists to prevent.

So most of what follows tests the GRAMMAR, with the executors driven through
fakes. `deltalake` and `pyarrow` are imported lazily inside functions, which is
what makes this possible without either installed.
"""
import pathlib
import sys
import types

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "spark_agent"))

import delta_ops as d  # noqa: E402


@pytest.fixture(autouse=True)
def clean_registry():
    d.forget_all()
    yield
    d.forget_all()


# --- the location registry ----------------------------------------------------

def test_a_remembered_table_is_found_by_bare_and_qualified_name():
    d.remember("orders", "abfss://lake/Tables/orders", "silver")
    assert d.known_location("orders") == "abfss://lake/Tables/orders"
    assert d.known_location("silver.orders") == "abfss://lake/Tables/orders"


def test_a_three_part_name_resolves_against_a_two_part_registration():
    # `catalog.schema.table` addressed against a two-part registration — match
    # on the last component rather than failing over spelling.
    d.remember("orders", "abfss://lake/Tables/orders", "silver")
    assert d.known_location("spark_catalog.silver.orders") == "abfss://lake/Tables/orders"


def test_an_unknown_table_has_no_location():
    assert d.known_location("nope") is None


def test_schema_locations_are_recorded_separately():
    d.remember_schema("gold", "abfss://lake/Tables/gold")
    assert d.known_schema_location("gold") == "abfss://lake/Tables/gold"
    assert d.known_schema_location("silver") is None


def test_remember_ignores_incomplete_pairs():
    d.remember("", "loc")
    d.remember("t", "")
    d.remember_schema("s", "")
    assert d.known_location("t") is None and d.known_schema_location("s") is None


# --- what is INTERCEPTED ------------------------------------------------------

@pytest.mark.parametrize("sql,kind", [
    ("OPTIMIZE delta.`abfss://lake/t`", "optimize"),
    ("optimize silver.orders", "optimize"),
    ("VACUUM delta.`abfss://lake/t`", "vacuum"),
    ("VACUUM t RETAIN 72 HOURS", "vacuum"),
    ("VACUUM t RETAIN 0.5 HOURS DRY RUN", "vacuum"),
])
def test_delta_only_statements_are_matched(sql, kind):
    got = d.match(sql)
    assert got and got[0] == kind, sql


@pytest.mark.parametrize("sql", [
    "SELECT * FROM orders",
    "CREATE TABLE t AS SELECT 1",          # one-part name: the engine's business
    "INSERT INTO t VALUES (1)",
    "MERGE INTO t x USING (SELECT 1) y ON x.a = y.a",  # subquery source
    "",
])
def test_everything_else_falls_through_to_the_engine(sql):
    # Falling through is the SAFE direction only because the engine then errors
    # honestly; matching something we cannot run faithfully is the unsafe one.
    assert d.match(sql) is None, sql


def test_a_merge_whose_branches_do_not_parse_falls_through():
    # A MERGE with a DELETE clause and no UPDATE/INSERT is one this grammar did
    # not really understand — running it as a no-op upsert would silently drop
    # the user's intent.
    assert d.match("MERGE INTO t x USING s y ON x.a = y.a "
                   "WHEN MATCHED THEN DELETE") is None


def test_ctas_is_intercepted_only_for_a_schema_we_registered():
    sql = "CREATE TABLE gold.fct AS SELECT * FROM silver.orders"
    assert d.match(sql) is None, "unregistered schema is the engine's own business"
    d.remember_schema("gold", "abfss://lake/Tables/gold")
    got = d.match(sql)
    assert got and got[0] == "ctas"


def test_ctas_or_replace_is_captured():
    d.remember_schema("gold", "abfss://lake/Tables/gold")
    _, params = d.match("CREATE OR REPLACE TABLE gold.fct AS SELECT 1")
    assert params["replace"]


def test_a_merge_with_both_branches_is_matched_with_its_parts():
    _, p = d.match(
        "MERGE INTO silver.orders AS t USING updates AS s ON t.id = s.id "
        "WHEN MATCHED AND s.op = 'U' THEN UPDATE SET t.amt = s.amt "
        "WHEN NOT MATCHED THEN INSERT (id, amt) VALUES (s.id, s.amt)")
    assert p["talias"] == "t" and p["salias"] == "s"
    assert p["source"] == "updates"
    assert "t.id = s.id" in p["on"]
    assert p["mcond"].strip() == "s.op = 'U'"
    assert "t.amt = s.amt" in p["sets"]
    assert p["icols"].strip() == "id, amt"


# --- the parsing helpers ------------------------------------------------------

def test_backticks_become_double_quotes_for_datafusion():
    # DataFusion lowercases bare identifiers, so a column like `Strm` must
    # arrive quoted or it will not resolve.
    assert d._dequote("`Strm` = s.`Strm`") == '"Strm" = s."Strm"'


def test_split_top_ignores_separators_inside_parens_and_quotes():
    # An assignment list may hold both: `a = coalesce(x, y), b = 'p,q'`.
    assert d._split_top("a = coalesce(x, y), b = 'p,q'") == ["a = coalesce(x, y)", "b = 'p,q'"]


def test_split_top_on_a_simple_list():
    assert d._split_top("a, b, c") == ["a", "b", "c"]


@pytest.mark.parametrize("ref,expected", [
    ("t.amount", "amount"),                 # the ordinary form
    ("`t`.`amount`", "amount"),             # per-segment quoting — this was BROKEN
    ('"t"."amount"', "amount"),             # the double-quoted spelling
    ("`t`.amount", "amount"),               # mixed
    ("t.`amount`", "amount"),               # mixed the other way
    ("  T.amount  ", "amount"),             # alias match is case-insensitive
    ("amount", "amount"),                   # unqualified
    ("`amount`", "amount"),
    ("s.amount", "s.amount"),               # a DIFFERENT alias is not stripped
    ("`t.amount`", "amount"),               # one quoted identifier: old reading kept
])
def test_plain_column_strips_the_target_alias(ref, expected):
    # delta-rs wants the target column's own name on the left of an update, so a
    # leading target alias has to go — in every spelling Spark SQL accepts.
    # `\`t\`.\`amount\`` used to yield "t`.`amount": a column no table has,
    # handed to delta-rs as a wrong answer rather than a refusal.
    assert d._plain_column(ref, "t") == expected


def test_a_dot_inside_quotes_belongs_to_the_name_not_the_path():
    # `my.col` is ONE column whose name contains a dot. Splitting it would
    # invent a qualifier that was never written.
    assert d._plain_column("`my.col`", "t") == "my.col"
    assert d._identifier_parts("`my.col`") == ["my.col"]
    assert d._identifier_parts("`t`.`my.col`") == ["t", "my.col"]


def test_identifier_parts_never_returns_nothing():
    # A caller indexes [0]; an empty list would be an IndexError naming this
    # file rather than the malformed input.
    assert d._identifier_parts("") == [""]
    assert d._identifier_parts("``") == [""]


def test_a_backticked_merge_maps_to_real_column_names(fake_deltalake):
    # The end-to-end shape of the bug: a MERGE written with quoted identifiers
    # must reach delta-rs with the target's own column names as update keys.
    spark = types.SimpleNamespace(
        table=lambda n: types.SimpleNamespace(toArrow=lambda: "arrow"))
    _, params = d.match(
        "MERGE INTO silver.orders AS t USING updates AS s ON t.id = s.id "
        "WHEN MATCHED THEN UPDATE SET `t`.`amt` = s.amt "
        "WHEN NOT MATCHED THEN INSERT (`t`.`id`, `t`.`amt`) VALUES (s.id, s.amt)")
    d.execute_merge(spark, params, lambda n: "abfss://lake/orders")
    merger = FakeDeltaTable.last.merger
    assert merger.updates == {"amt": "s.amt"}
    assert merger.inserts == {"id": "s.id", "amt": "s.amt"}


def test_table_uri_prefers_an_explicit_delta_path():
    assert d._table_uri("delta.`abfss://lake/t`", lambda n: "never") == "abfss://lake/t"


def test_table_uri_resolves_a_bare_name_through_the_caller():
    assert d._table_uri("silver.orders", lambda n: f"resolved:{n}") == "resolved:silver.orders"


def test_storage_options_may_be_a_callable_read_at_statement_time():
    # The callable form is what the agent uses: a refreshed bearer is picked up
    # without restarting the session.
    assert d._resolve_options(lambda: {"bearer": "fresh"}) == {"bearer": "fresh"}
    assert d._resolve_options({"a": 1}) == {"a": 1}
    assert d._resolve_options(None) == {}
    assert d._resolve_options(lambda: None) == {}


# --- refusals that protect meaning -------------------------------------------

@pytest.fixture
def fake_deltalake(monkeypatch):
    mod = types.ModuleType("deltalake")
    mod.DeltaTable = FakeDeltaTable
    monkeypatch.setitem(sys.modules, "deltalake", mod)
    return mod


@pytest.mark.parametrize("clause", ["WHERE part = 'x'", "ZORDER BY (id)"])
def test_optimize_with_a_narrowing_clause_is_refused_not_silently_widened(clause, fake_deltalake):
    # Executing a bare compaction while the user asked for something narrower
    # would compact MORE than requested — a silent semantic change.
    kind, params = d.match(f"OPTIMIZE silver.orders {clause}")
    with pytest.raises(d.DeltaOpError, match="not supported"):
        d.execute(kind, params, lambda n: "uri")


def test_the_refusal_names_the_jvm_overlay_as_the_way_out(fake_deltalake):
    kind, params = d.match("OPTIMIZE t ZORDER BY (id)")
    with pytest.raises(d.DeltaOpError, match="JVM overlay"):
        d.execute(kind, params, lambda n: "uri")


# --- executors, with delta-rs faked ------------------------------------------

class FakeMerger:
    def __init__(self, metrics):
        self.metrics = metrics
        self.updates = None
        self.inserts = None
        self.predicate = None

    def when_matched_update(self, updates, predicate=None):
        self.updates, self.predicate = updates, predicate
        return self

    def when_not_matched_insert(self, updates):
        self.inserts = updates
        return self

    def execute(self):
        return self.metrics


class FakeDeltaTable:
    last = None

    def __init__(self, uri, storage_options=None):
        FakeDeltaTable.last = self
        self.uri = uri
        self.storage_options = storage_options
        self.optimize = types.SimpleNamespace(
            compact=lambda: {"numFilesRemoved": 4, "numFilesAdded": 1})
        self.vacuum_args = None
        self.merger = FakeMerger({"num_target_rows_updated": 2,
                                  "num_target_rows_inserted": 3})

    def vacuum(self, retention_hours, dry_run, enforce_retention_duration):
        self.vacuum_args = (retention_hours, dry_run, enforce_retention_duration)
        return ["f1", "f2"]

    def merge(self, source, predicate, source_alias, target_alias):
        self.merge_call = {"source": source, "predicate": predicate,
                           "source_alias": source_alias, "target_alias": target_alias}
        return self.merger


def test_optimize_reports_the_compaction(fake_deltalake):
    kind, params = d.match("OPTIMIZE delta.`abfss://lake/t`")
    assert d.execute(kind, params, lambda n: n) == \
        "OPTIMIZE: compacted 4 file(s) into 1 (delta-rs)"


def test_vacuum_defaults_to_the_delta_retention(fake_deltalake):
    kind, params = d.match("VACUUM delta.`abfss://lake/t`")
    out = d.execute(kind, params, lambda n: n)
    assert "deleted 2 file(s), retain 168h" in out


def test_a_fractional_retention_rounds_DOWN(fake_deltalake):
    # Spark's RETAIN accepts fractions; delta-rs takes whole hours. Rounding UP
    # would retain longer than asked — but rounding down deletes files the user
    # wanted kept, so the code rounds down and this pins which way.
    kind, params = d.match("VACUUM t RETAIN 0.9 HOURS")
    d.execute(kind, params, lambda n: "uri")
    assert FakeDeltaTable.last.vacuum_args[0] == 0


def test_dry_run_says_would_delete(fake_deltalake):
    kind, params = d.match("VACUUM t RETAIN 24 HOURS DRY RUN")
    out = d.execute(kind, params, lambda n: "uri")
    assert "would delete" in out
    assert FakeDeltaTable.last.vacuum_args[1] is True


def test_merge_maps_assignments_to_bare_columns(fake_deltalake):
    spark = types.SimpleNamespace(
        table=lambda name: types.SimpleNamespace(toArrow=lambda: f"arrow:{name}"))
    _, params = d.match(
        "MERGE INTO silver.orders AS t USING updates AS s ON t.id = s.id "
        "WHEN MATCHED THEN UPDATE SET t.amt = s.amt "
        "WHEN NOT MATCHED THEN INSERT (id, amt) VALUES (s.id, s.amt)")
    out = d.execute_merge(spark, params, lambda n: "abfss://lake/orders")
    merger = FakeDeltaTable.last.merger
    assert merger.updates == {"amt": "s.amt"}, "the target alias must be stripped"
    assert merger.inserts == {"id": "s.id", "amt": "s.amt"}
    assert FakeDeltaTable.last.merge_call["source"] == "arrow:updates"
    assert out == "MERGE: updated 2 row(s), inserted 3 (delta-rs)"


def test_merge_refuses_an_insert_whose_arity_does_not_line_up(fake_deltalake):
    spark = types.SimpleNamespace(
        table=lambda n: types.SimpleNamespace(toArrow=lambda: "arrow"))
    _, params = d.match(
        "MERGE INTO t AS t USING s AS s ON t.id = s.id "
        "WHEN NOT MATCHED THEN INSERT (id, amt) VALUES (s.id)")
    with pytest.raises(d.DeltaOpError, match="2 column\\(s\\) but 1 value"):
        d.execute_merge(spark, params, lambda n: "uri")


def test_merge_refuses_an_unparseable_assignment(fake_deltalake):
    spark = types.SimpleNamespace(
        table=lambda n: types.SimpleNamespace(toArrow=lambda: "arrow"))
    _, params = d.match(
        "MERGE INTO t AS t USING s AS s ON t.id = s.id "
        "WHEN MATCHED THEN UPDATE SET amt "
        "WHEN NOT MATCHED THEN INSERT (id) VALUES (s.id)")
    with pytest.raises(d.DeltaOpError, match="cannot parse assignment"):
        d.execute_merge(spark, params, lambda n: "uri")


# --- CTAS: the false-green write this module exists to prevent ---------------

class FakeWriter:
    def __init__(self, sink):
        self.sink = sink

    def format(self, fmt):
        self.sink["format"] = fmt
        return self

    def mode(self, m):
        self.sink["mode"] = m
        return self

    def save(self, path):
        self.sink["path"] = path


def test_ctas_lands_at_the_schema_location_not_the_engine_warehouse():
    d.remember_schema("gold", "abfss://lake/Tables/gold")
    sink, ran = {}, []

    def original_sql(q):
        ran.append(q)
        return types.SimpleNamespace(write=FakeWriter(sink))

    _, params = d.match("CREATE TABLE gold.fct AS SELECT * FROM silver.o")
    out = d.execute_ctas(None, original_sql, params)
    assert sink["path"] == "abfss://lake/Tables/gold/fct"
    assert sink["format"] == "delta"
    assert sink["mode"] == "errorifexists"
    assert any("CREATE TABLE IF NOT EXISTS" in q for q in ran), "must re-register the name"
    assert d.known_location("fct") == "abfss://lake/Tables/gold/fct"
    assert "gold.fct" in out


def test_ctas_or_replace_overwrites_and_drops_the_stale_registration():
    d.remember_schema("gold", "abfss://lake/Tables/gold")
    sink, ran = {}, []

    def original_sql(q):
        ran.append(q)
        return types.SimpleNamespace(write=FakeWriter(sink))

    _, params = d.match("CREATE OR REPLACE TABLE gold.fct AS SELECT 1")
    d.execute_ctas(None, original_sql, params)
    assert sink["mode"] == "overwrite"
    assert any("DROP TABLE IF EXISTS" in q for q in ran)


def test_a_failing_drop_does_not_block_the_re_register():
    # A stale catalog entry must not stop the table being usable.
    d.remember_schema("gold", "abfss://lake/Tables/gold")
    sink, ran = {}, []

    def original_sql(q):
        ran.append(q)
        if q.startswith("DROP"):
            raise RuntimeError("no such table")
        return types.SimpleNamespace(write=FakeWriter(sink))

    _, params = d.match("CREATE OR REPLACE TABLE gold.fct AS SELECT 1")
    d.execute_ctas(None, original_sql, params)
    assert any("CREATE TABLE IF NOT EXISTS" in q for q in ran)


# --- install: the interception itself ----------------------------------------

class FakeSpark:
    def __init__(self, rows=None):
        self.queries = []
        self._rows = rows if rows is not None else [{"location": "abfss://engine/t"}]
        self.frames = []

    def sql(self, query, *a, **kw):
        self.queries.append(query)
        return types.SimpleNamespace(collect=lambda: self._rows,
                                     write=FakeWriter({}))

    def createDataFrame(self, data, schema=None):  # noqa: N802 — pyspark's name
        self.frames.append((data, schema))
        return f"DF{data}"

    def table(self, name):
        return types.SimpleNamespace(toArrow=lambda: f"arrow:{name}")


def test_install_leaves_unmatched_sql_with_the_engine():
    spark = FakeSpark()
    d.install(spark, storage_options={})
    spark.sql("SELECT 1")
    assert spark.queries == ["SELECT 1"]


def test_install_routes_a_matched_statement_and_returns_a_dataframe(fake_deltalake):
    # A DataFrame, so callers can .show()/.collect() as after a native OPTIMIZE
    # rather than getting None back.
    spark = FakeSpark()
    d.install(spark, storage_options={})
    out = spark.sql("OPTIMIZE delta.`abfss://lake/t`")
    assert out.startswith("DF")
    assert spark.frames[0][1] == ["result"]
    assert "compacted" in spark.frames[0][0][0][0]


def test_install_returns_the_original_sql_so_it_can_be_restored():
    spark = FakeSpark()
    original = d.install(spark, storage_options={})
    assert spark.sql is not original
    spark.sql = original
    spark.sql("OPTIMIZE t")
    assert spark.queries == ["OPTIMIZE t"]


def test_a_non_string_query_is_passed_straight_through():
    spark = FakeSpark()
    d.install(spark, storage_options={})
    spark.sql(object())
    assert len(spark.queries) == 1


def test_resolve_prefers_the_recorded_location_over_asking_the_engine(fake_deltalake, capsys):
    d.remember("t", "abfss://lake/Tables/t")
    spark = FakeSpark()
    d.install(spark, storage_options={})
    spark.sql("OPTIMIZE t")
    assert FakeDeltaTable.last.uri == "abfss://lake/Tables/t"
    assert "DESCRIBE DETAIL" not in " ".join(spark.queries)


def test_an_unrecorded_table_falls_back_loudly(fake_deltalake, capsys):
    # The fallback announces itself: everything that went wrong around this code
    # went wrong quietly.
    spark = FakeSpark()
    d.install(spark, storage_options={})
    spark.sql("OPTIMIZE unregistered")
    assert "no recorded location" in capsys.readouterr().err
    assert any("DESCRIBE DETAIL" in q for q in spark.queries)


def test_a_describe_that_answers_with_no_rows_is_an_error_not_an_indexerror(fake_deltalake):
    # An engine can answer a DESCRIBE with the right schema and no rows, and
    # raise nothing — which used to surface as an IndexError naming this file
    # rather than the cause.
    spark = FakeSpark(rows=[])
    d.install(spark, storage_options={})
    with pytest.raises(d.DeltaOpError, match="answered DESCRIBE DETAIL with no rows"):
        spark.sql("OPTIMIZE unregistered")


def test_install_exposes_the_change_feed_helper(monkeypatch):
    # Exposed as a helper rather than by intercepting spark.read: silently
    # rewriting a user's read chain would hide which engine answered.
    spark = FakeSpark()
    d.install(spark, storage_options={"o": 1})
    seen = {}
    monkeypatch.setattr(d, "read_change_feed",
                        lambda s, uri, **kw: seen.update(kw, uri=uri) or "frame")
    assert spark.delta_change_feed("abfss://lake/t") == "frame"
    assert seen["storage_options"] == {"o": 1}


# --- change feed --------------------------------------------------------------

def test_an_empty_change_feed_says_how_to_enable_it(monkeypatch, fake_deltalake):
    pa = pytest.importorskip("pyarrow")
    monkeypatch.setitem(sys.modules, "pyarrow", pa)
    empty = pa.table({"a": pa.array([], type=pa.int64())})

    class CDFTable(FakeDeltaTable):
        def load_cdf(self, starting_version=0, ending_version=None):
            return types.SimpleNamespace(read_all=lambda: empty)

    fake_deltalake.DeltaTable = CDFTable
    with pytest.raises(d.DeltaOpError, match=r"delta\.enableChangeDataFeed"):
        d.read_change_feed(FakeSpark(), "abfss://lake/t")


# --- DESCRIBE, answered from the Delta log -----------------------------------
#
# The failure this replaces: Sail returns the right SHAPE and no rows for
# DESCRIBE, so an empty answer that raises nothing reads as "the table has no
# columns" rather than "the engine did not implement this".

@pytest.mark.parametrize("sql,kind", [
    ("DESCRIBE DETAIL silver.orders", "describe_detail"),
    ("describe detail delta.`abfss://lake/t`", "describe_detail"),
    ("DESCRIBE TABLE silver.orders", "describe_table"),
    ("DESCRIBE silver.orders", "describe_table"),
    ("DESC silver.orders", "describe_table"),
    ("DESCRIBE delta.`abfss://lake/t`", "describe_table"),
])
def test_describe_statements_are_intercepted(sql, kind):
    # DESCRIBE TABLE is answered only for a table this emulator can LOCATE.
    d.remember("orders", "abfss://lake/Tables/orders", "silver")
    got = d.match(sql)
    assert got and got[0] == kind, sql


@pytest.mark.parametrize("sql", [
    "DESCRIBE my_temp_view",     # a temp view is the engine's own business
    "DESCRIBE some_function",
    "DESC unregistered.table",
])
def test_describe_of_something_we_cannot_locate_falls_through(sql):
    # Answering these from the Delta log would mean inventing a table.
    assert d.match(sql) is None, sql


def test_describe_detail_wins_over_describe_table():
    # `DESCRIBE DETAIL x` also matches the looser DESCRIBE pattern; order in
    # `match` is what keeps it from being answered with a column list.
    assert d.match("DESCRIBE DETAIL x")[0] == "describe_detail"


class Field:
    def __init__(self, name, type_):
        self.name, self.type = name, type_


def describe_table_stub(fields, partitions=(), version=3, files=2):
    class T(FakeDeltaTable):
        def metadata(self):
            return types.SimpleNamespace(
                id="abc-123", name="orders", description="",
                partition_columns=list(partitions), configuration={"delta.enableCDF": "true"})

        def version(self):
            return version

        def file_uris(self):
            return ["f"] * files

        def schema(self):
            return types.SimpleNamespace(fields=fields)
    return T


def test_describe_detail_reports_the_log_not_a_guess(fake_deltalake):
    fake_deltalake.DeltaTable = describe_table_stub([])
    spark = FakeSpark()
    _, params = d.match("DESCRIBE DETAIL delta.`abfss://lake/t`")
    d.describe_detail(spark, params, lambda n: n)
    rows, cols = spark.frames[0]
    assert cols == ["format", "id", "name", "description", "location",
                    "version", "numFiles", "partitionColumns", "properties"]
    (fmt, ident, name, _desc, loc, version, numfiles, parts, props) = rows[0]
    assert fmt == "delta" and ident == "abc-123" and name == "orders"
    assert loc == "abfss://lake/t"
    assert (version, numfiles) == (3, 2)
    assert parts == [] and props == {"delta.enableCDF": "true"}


def test_describe_detail_omits_columns_it_cannot_answer_truthfully(fake_deltalake):
    # sizeInBytes and the reader/writer versions are LEFT OUT rather than filled
    # with a plausible number — the documented subset is the honest answer.
    fake_deltalake.DeltaTable = describe_table_stub([])
    spark = FakeSpark()
    _, params = d.match("DESCRIBE DETAIL t")
    d.describe_detail(spark, params, lambda n: "uri")
    assert "sizeInBytes" not in spark.frames[0][1]
    assert not any("Version" in c for c in spark.frames[0][1])


def test_describe_table_returns_the_real_column_list(fake_deltalake):
    fake_deltalake.DeltaTable = describe_table_stub(
        [Field("id", 'PrimitiveType("long")'), Field("region", 'PrimitiveType("string")')],
        partitions=["region"])
    d.remember("t", "abfss://lake/Tables/t")
    spark = FakeSpark()
    _, params = d.match("DESCRIBE TABLE t")
    d.describe_table(spark, params, lambda n: "uri")
    rows, cols = spark.frames[0]
    assert cols == ["col_name", "data_type", "comment"]
    assert rows == [("id", "bigint", ""), ("region", "string", "partition")]


def test_a_delta_log_declaring_no_columns_is_an_error_not_an_empty_answer(fake_deltalake):
    # The exact shape being replaced: no rows and no error reads as "no columns".
    fake_deltalake.DeltaTable = describe_table_stub([])
    d.remember("t", "abfss://lake/Tables/t")
    spark = FakeSpark()
    _, params = d.match("DESCRIBE t")
    with pytest.raises(d.DeltaOpError, match="declares no columns"):
        d.describe_table(spark, params, lambda n: "uri")


@pytest.mark.parametrize("raw,expected", [
    ('PrimitiveType("long")', "bigint"),
    ('PrimitiveType("string")', "string"),
    ('PrimitiveType("integer")', "int"),
    ("StructType(...)", "StructType(...)"),   # structured types pass through
])
def test_spark_type_renders_primitives_and_passes_the_rest_through(raw, expected):
    # A wrong nested type would be worse than an unfamiliar one, so anything
    # structured is delta-rs's own rendering rather than a guess.
    assert d._spark_type(raw) == expected


def test_install_routes_describe_through_the_delta_log(fake_deltalake):
    fake_deltalake.DeltaTable = describe_table_stub([Field("id", 'PrimitiveType("long")')])
    d.remember("t", "abfss://lake/Tables/t")
    spark = FakeSpark()
    d.install(spark, storage_options={})
    spark.sql("DESCRIBE TABLE t")
    assert spark.frames, "DESCRIBE must be answered here, not passed to the engine"
    assert spark.frames[-1][1] == ["col_name", "data_type", "comment"]
