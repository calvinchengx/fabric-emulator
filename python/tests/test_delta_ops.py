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
    mod.write_deltalake = lambda *a, **k: None
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

    def when_not_matched_insert_all(self, predicate=None, except_cols=None):
        self.inserts = "*"
        self.insert_all = {"predicate": predicate, "except_cols": except_cols}
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

    def metadata(self):
        return types.SimpleNamespace(configuration=getattr(self, "configuration", {}))


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


def test_merge_subquery_source_is_run_on_the_engine_not_looked_up_as_a_table(fake_deltalake):
    ran = []

    def sql(q):
        ran.append(q)
        return types.SimpleNamespace(toArrow=lambda: "arrow:subquery")

    spark = types.SimpleNamespace(
        sql=sql,
        table=lambda n: (_ for _ in ()).throw(AssertionError(f"named lookup of {n}")))
    _, params = d.match(
        "MERGE INTO t AS t "
        "USING (SELECT * FROM VALUES (1, 'b') AS s(id, v)) AS s "
        "ON t.id = s.id "
        "WHEN MATCHED THEN UPDATE SET t.v = s.v "
        "WHEN NOT MATCHED THEN INSERT *")
    d.execute_merge(spark, params, lambda n: "uri")
    assert ran and "SELECT" in ran[0].upper()
    merger = FakeDeltaTable.last.merger
    assert FakeDeltaTable.last.merge_call["source"] == "arrow:subquery"
    assert merger.updates == {"v": "s.v"}
    assert merger.inserts == "*"


def test_merge_path_target_does_not_strip_the_delta_uri(fake_deltalake):
    spark = types.SimpleNamespace(
        sql=lambda q: types.SimpleNamespace(toArrow=lambda: "arrow"),
        table=lambda n: types.SimpleNamespace(toArrow=lambda: "arrow"))
    _, params = d.match(
        "MERGE INTO delta.`/tmp/t` AS t "
        "USING (SELECT 1 AS id, 'b' AS v) AS s "
        "ON t.id = s.id "
        "WHEN NOT MATCHED THEN INSERT *")
    d.execute_merge(spark, params, lambda n: f"resolved:{n}")
    assert FakeDeltaTable.last.uri == "/tmp/t"


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


_deltalake = types.ModuleType("deltalake")
HAVE_DELTA_RS = False
try:
    import deltalake as _imported_deltalake
except ImportError:  # pragma: no cover - a venv without the test group's extras
    pass
else:
    _deltalake = _imported_deltalake
    HAVE_DELTA_RS = True

needs_delta_rs = pytest.mark.skipif(not HAVE_DELTA_RS, reason="deltalake not installed")


@needs_delta_rs
def test_probe_shaped_merge_upserts_a_real_delta_log(tmp_path):
    """The engine-matrix MERGE probe, against a real _delta_log.

    Grammar tests can go green on a matcher that never writes. This is the
    outcome the matrix cell claims: one row updated, one inserted, files on disk.
    """
    import pyarrow as pa
    from deltalake import DeltaTable, write_deltalake

    path = str(tmp_path / "t_merge")
    write_deltalake(path, pa.table({
        "id": pa.array([1], pa.int64()),
        "v": pa.array(["a"]),
    }))
    incoming = pa.table({
        "id": pa.array([1, 2], pa.int64()),
        "v": pa.array(["b", "c"]),
    })
    spark = types.SimpleNamespace(
        sql=lambda q: types.SimpleNamespace(toArrow=lambda: incoming),
        table=lambda n: (_ for _ in ()).throw(AssertionError("named source")),
    )
    kind, params = d.match(
        f"MERGE INTO delta.`{path}` AS t "
        "USING (SELECT * FROM VALUES (1, 'b'), (2, 'c') AS s(id, v)) AS s "
        "ON t.id = s.id "
        "WHEN MATCHED THEN UPDATE SET t.v = s.v "
        "WHEN NOT MATCHED THEN INSERT *")
    assert kind == "merge"
    d.execute_merge(spark, params, lambda n: n, storage_options={})
    got = DeltaTable(path).to_pandas().sort_values("id").reset_index(drop=True)
    assert list(got["id"]) == [1, 2]
    assert list(got["v"]) == ["b", "c"]


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
    # Was `errorifexists`, the semantically right mode for a plain CREATE TABLE
    # — and one Sail does not implement (`[UNSUPPORTED_OPERATION] errorifexists
    # is not supported`), which the engine matrix surfaced once honouring an
    # explicit LOCATION made this path reachable. The guarantee is now enforced
    # by asking delta-rs whether the table is already there (below), through a
    # route both engines have.
    assert sink["mode"] == "overwrite"
    assert any("CREATE TABLE IF NOT EXISTS" in q for q in ran), "must re-register the name"
    assert d.known_location("fct") == "abfss://lake/Tables/gold/fct"
    assert "gold.fct" in out


@needs_delta_rs
def test_plain_create_table_refuses_an_existing_delta_table(monkeypatch):
    """The guarantee `errorifexists` used to give, enforced portably.

    A plain CREATE TABLE must not silently overwrite. Sail cannot execute the
    write mode that says so, so the check moved to delta-rs — and it must still
    refuse, or the mode swap would have quietly turned CREATE into REPLACE.
    """
    d.remember_schema("gold", "abfss://lake/Tables/gold")
    monkeypatch.setattr(d, "_resolve_options", lambda _o: {})
    monkeypatch.setattr(_deltalake.DeltaTable, "is_deltatable",
                        staticmethod(lambda *_a, **_k: True))

    _, params = d.match("CREATE TABLE gold.fct AS SELECT * FROM silver.o")
    with pytest.raises(d.DeltaOpError, match="already exists"):
        d.execute_ctas(None, lambda q: None, params)


@needs_delta_rs
def test_an_unreadable_location_is_not_treated_as_an_existing_table(monkeypatch):
    """A credential failure must not be reported as "table already exists".

    That would send someone to entirely the wrong problem. The write itself
    surfaces any real storage fault a moment later.
    """
    d.remember_schema("gold", "abfss://lake/Tables/gold")
    def boom(*_a, **_k):
        raise OSError("Account must be specified")

    monkeypatch.setattr(_deltalake.DeltaTable, "is_deltatable", staticmethod(boom))
    sink = {}
    _, params = d.match("CREATE TABLE gold.fct AS SELECT * FROM silver.o")
    out = d.execute_ctas(None, lambda q: types.SimpleNamespace(write=FakeWriter(sink)), params)
    assert sink["mode"] == "overwrite"
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


def test_an_engine_that_cannot_parse_describe_detail_names_the_real_cause(fake_deltalake):
    # Sail has no DETAIL in its DESCRIBE grammar, so on the one engine this
    # module is installed for, the fallback is EXPECTED to raise. Uncaught, the
    # engine's own parse error surfaced instead — `found DETAIL at 9:15` —
    # pointing at column 9 of a statement the user never wrote, which sent
    # every reader looking at their own MERGE rather than at the missing
    # registration. databricks-emulator lost a release to that message.
    class Unparseable(FakeSpark):
        def sql(self, query, *a, **kw):
            self.queries.append(query)
            raise RuntimeError("invalid argument: found DETAIL at 9:15 expected "
                               "'FUNCTION', 'CATALOG', 'DATABASE', ...")

    spark = Unparseable()
    d.install(spark, storage_options={})
    with pytest.raises(d.DeltaOpError) as excinfo:
        spark.sql("OPTIMIZE unregistered")
    said = str(excinfo.value)
    assert "unregistered" in said          # which table
    assert "not registered" in said        # why it had to ask at all
    assert "found DETAIL" in said          # the engine's own words, kept


def test_a_describe_that_answers_with_no_rows_is_an_error_not_an_indexerror(fake_deltalake):
    # An engine can answer a DESCRIBE with the right schema and no rows, and
    # raise nothing — which used to surface as an IndexError naming this file
    # rather than the cause.
    spark = FakeSpark(rows=[])
    d.install(spark, storage_options={})
    with pytest.raises(d.DeltaOpError, match="answered DESCRIBE DETAIL with no rows"):
        spark.sql("OPTIMIZE unregistered")


def test_install_exposes_the_change_feed_helper(monkeypatch):
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


def test_read_change_feed_casts_uint64_so_spark_connect_can_ingest_it(
        monkeypatch, fake_deltalake):
    # Connect: [UNSUPPORTED_DATA_TYPE_FOR_ARROW_CONVERSION] uint64. delta-rs
    # types _commit_version as unsigned; the engine-matrix probe died here
    # after both halves of the intercept had already run.
    pa = pytest.importorskip("pyarrow")
    monkeypatch.setitem(sys.modules, "pyarrow", pa)
    feed = pa.table({
        "id": pa.array([1], type=pa.int64()),
        "_change_type": pa.array(["insert"]),
        "_commit_version": pa.array([0], type=pa.uint64()),
    })

    class CDFTable(FakeDeltaTable):
        def load_cdf(self, starting_version=0, ending_version=None):
            return types.SimpleNamespace(read_all=lambda: feed)

    fake_deltalake.DeltaTable = CDFTable
    spark = FakeSpark()
    d.read_change_feed(spark, "abfss://lake/t")
    data, _schema = spark.frames[0]
    assert "uint" not in str(data["_commit_version"].dtype)
    assert list(data["_change_type"]) == ["insert"]


def test_cdf_option_truthy_accepts_spark_spellings():
    assert d._opt_truthy("true") and d._opt_truthy("TRUE") and d._opt_truthy("1")
    assert not d._opt_truthy("false") and not d._opt_truthy(None) and not d._opt_truthy("")


def test_cdf_write_intercepts_only_the_named_shapes():
    # The option the notebook set, or a table that already has the feature.
    # Parquet, partitionBy, and a plain Delta overwrite stay on the engine.
    assert d.cdf_write_should_intercept(
        "delta", "/tmp/t", {"delta.enableChangeDataFeed": "true"}, None, {})
    assert not d.cdf_write_should_intercept(
        "parquet", "/tmp/t", {"delta.enableChangeDataFeed": "true"}, None, {})
    assert not d.cdf_write_should_intercept(
        "delta", "/tmp/t", {}, ["part"], {})
    assert not d.cdf_write_should_intercept("delta", "/tmp/nope", {}, None, {})


@needs_delta_rs
def test_probe_shaped_cdf_write_and_read_carry_change_type(tmp_path):
    """The engine-matrix CDF probe, against a real _delta_log.

    Overwrite with the enable option, append without it, read startingVersion=0.
    Both halves must be this module: a write-only intercept leaves the read
    inert, a read-only intercept has no feed to serve.
    """
    import pyarrow as pa

    path = str(tmp_path / "t_cdf")
    v0 = types.SimpleNamespace(toArrow=lambda: pa.table({"id": pa.array([1], pa.int64())}))
    v1 = types.SimpleNamespace(toArrow=lambda: pa.table({"id": pa.array([2], pa.int64())}))
    d.write_cdf_table(v0, path, mode="overwrite", enable=True, storage_options={})
    assert d.table_has_cdf(path, {})
    assert d.cdf_write_should_intercept("delta", path, {}, None, {})
    d.write_cdf_table(v1, path, mode="append", enable=False, storage_options={})

    spark = FakeSpark()
    d.read_change_feed(spark, path, starting_version=0, storage_options={})
    data, _schema = spark.frames[0]
    assert "_change_type" in list(data.columns)
    assert "insert" in set(data["_change_type"])
    assert set(data["id"]) == {1, 2}
    # Spark Connect rejects uint64; the feed must not hand that width through.
    if "_commit_version" in data.columns:
        assert "uint" not in str(data["_commit_version"].dtype)


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
    rows, schema = spark.frames[0]
    # A list of names makes Spark Connect infer types. Empty partitionColumns
    # / properties then raise CANNOT_DETERMINE_TYPE — the engine-matrix probe.
    assert isinstance(schema, str), schema
    assert "ARRAY<STRING>" in schema.upper()
    assert "MAP<STRING,STRING>" in schema.upper().replace(" ", "")
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


def test_create_table_using_delta_location_is_remembered_so_describe_can_find_it():
    # The engine-matrix DESCRIBE probes (and a notebook CREATE TABLE cell) never
    # call remember() themselves. The statement named its LOCATION; recording
    # that is not a guess. Without this, DESCRIBE TABLE d_reg falls through to
    # Sail's zero-row answer and the middle matrix column stays red.
    spark = FakeSpark()
    d.install(spark, storage_options={})
    spark.sql("CREATE TABLE IF NOT EXISTS d_reg USING delta LOCATION '/tmp/t_describe'")
    assert d.known_location("d_reg") == "/tmp/t_describe"
    kind, _ = d.match("DESCRIBE TABLE d_reg")
    assert kind == "describe_table"


def test_create_table_using_parquet_location_is_not_remembered():
    spark = FakeSpark()
    d.install(spark, storage_options={})
    spark.sql("CREATE TABLE p_reg USING parquet LOCATION '/tmp/p'")
    assert d.known_location("p_reg") is None
    assert d.match("DESCRIBE TABLE p_reg") is None


def test_a_failed_create_table_is_not_remembered():
    class Boom(FakeSpark):
        def sql(self, query, *a, **kw):
            raise RuntimeError("catalog refused")

    spark = Boom()
    d.install(spark, storage_options={})
    with pytest.raises(RuntimeError, match="catalog refused"):
        spark.sql("CREATE TABLE d_reg USING delta LOCATION '/tmp/t'")
    assert d.known_location("d_reg") is None


def _fake_pyspark_connect(monkeypatch, Reader, Writer, ConnectDF):
    rw = types.ModuleType("pyspark.sql.connect.readwriter")
    rw.DataFrameReader = Reader
    rw.DataFrameWriter = Writer
    dfmod = types.ModuleType("pyspark.sql.connect.dataframe")
    dfmod.DataFrame = ConnectDF
    monkeypatch.setitem(sys.modules, "pyspark", types.ModuleType("pyspark"))
    monkeypatch.setitem(sys.modules, "pyspark.sql", types.ModuleType("pyspark.sql"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect", types.ModuleType("pyspark.sql.connect"))
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.readwriter", rw)
    monkeypatch.setitem(sys.modules, "pyspark.sql.connect.dataframe", dfmod)


def test_cdf_notebook_api_wraps_load_and_save(monkeypatch, fake_deltalake, capsys):
    """The Connect reader/writer wrap is what the matrix probe actually calls."""
    written = []
    fake_deltalake.write_deltalake = (
        lambda path, table, **kw: written.append((path, table, kw)))

    class DataFrameReader:
        def format(self, fmt):
            self._format = fmt
            return self

        def load(self, path=None, format=None, schema=None, **options):
            return "engine-load"

    class DataFrameWriter:
        def __init__(self):
            self._write = types.SimpleNamespace(
                source="delta",
                options={"delta.enableChangeDataFeed": "true"},
                mode="overwrite",
            )
            self._df = types.SimpleNamespace()
            self._spark = None

        def format(self, fmt):
            self._write.source = fmt
            return self

        def mode(self, m):
            self._write.mode = m
            return self

        def save(self, path=None, format=None, mode=None, partitionBy=None, **options):
            return "engine-save"

    class ConnectDF:
        def __init__(self, df, spark):
            self._df, self._spark = df, spark

        def toArrow(self):  # noqa: N802 — pyspark's name
            return "arrow"

    _fake_pyspark_connect(monkeypatch, DataFrameReader, DataFrameWriter, ConnectDF)
    spark = FakeSpark()
    spark.read = DataFrameReader()
    d._install_cdf_dataframe_api(spark, storage_options={})

    feeds = []
    monkeypatch.setattr(d, "read_change_feed",
                        lambda *a, **k: feeds.append((a, k)) or "feed")
    out = spark.read.load("/tmp/t", format="delta", readChangeFeed="true",
                          startingVersion=0)
    assert out == "feed" and feeds
    assert "Change Data Feed read" in capsys.readouterr().err

    DataFrameWriter().save("/tmp/t")
    assert written and written[0][0] == "/tmp/t"
    assert written[0][2]["configuration"] == {"delta.enableChangeDataFeed": "true"}
    assert "Delta write with Change Data Feed" in capsys.readouterr().err

    # Parquet, and a partitioned Delta write, stay on the engine.
    parquet = DataFrameWriter()
    parquet._write.source = "parquet"
    assert parquet.save("/tmp/p") == "engine-save"
    assert spark.read.load("/tmp/t", format="parquet") == "engine-load"


def test_cdf_notebook_api_wrap_is_idempotent(monkeypatch):
    class DataFrameReader:
        def load(self, *a, **k):
            return "engine"

    class DataFrameWriter:
        def save(self, *a, **k):
            return "engine"

    _fake_pyspark_connect(monkeypatch, DataFrameReader, DataFrameWriter, lambda *a, **k: None)
    spark = FakeSpark()
    spark.read = DataFrameReader()
    d._install_cdf_dataframe_api(spark, {})
    first = DataFrameReader.load
    d._install_cdf_dataframe_api(spark, {})
    assert DataFrameReader.load is first


def test_write_cdf_table_enables_the_feature_only_when_asked(fake_deltalake):
    seen = []
    fake_deltalake.write_deltalake = lambda path, table, **kw: seen.append(kw)
    df = types.SimpleNamespace(toArrow=lambda: "arrow")
    d.write_cdf_table(df, "/tmp/t", mode="append", enable=False, storage_options={})
    assert "configuration" not in seen[0]
    d.write_cdf_table(df, "/tmp/t", mode=None, enable=True, storage_options={})
    assert seen[1]["mode"] == "overwrite"
    assert seen[1]["configuration"] == {"delta.enableChangeDataFeed": "true"}


def test_table_has_cdf_reads_the_log_property(fake_deltalake):
    class CDFTable(FakeDeltaTable):
        def metadata(self):
            return types.SimpleNamespace(
                configuration={"delta.enableChangeDataFeed": "true"})

    fake_deltalake.DeltaTable = CDFTable
    assert d.table_has_cdf("/tmp/t", {}) is True


def test_table_has_cdf_is_false_when_the_log_cannot_open(fake_deltalake, monkeypatch):
    class Boom:
        def __init__(self, *a, **k):
            raise RuntimeError("no table")

    fake_deltalake.DeltaTable = Boom
    assert d.table_has_cdf("/tmp/missing", {}) is False
