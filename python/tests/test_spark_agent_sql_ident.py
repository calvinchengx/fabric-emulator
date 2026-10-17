"""The SQL identifier boundary, asserted as a table -- and through the real paths.

WHY THIS FILE EXISTS, and it is the lesson the `tls_scope` follow-up paid for
rather than a general principle. When six unconditional TLS skips were repaired
onto a host-scoped helper, NOTHING asserted the helper: an 89-line module whose
whole job was deciding whether to authenticate a peer holding a client secret,
with its behaviour pinned by nothing, and it was wrong in the PERMISSIVE
direction twice over for two releases. `scripts/check_script_test_coverage.py`
scopes itself to `scripts/`, so no gate noticed.

`sqlident` is the same shape of thing one rule over: a few lines that decide
whether a name can become statement, on the path that installs OneLake row- and
column-level security. So it ships with a table, and the table runs through the
HELPER and through the REAL CALL PATHS both.

WHY BOTH HALVES, concretely. A `quote_ident` that is perfect proves nothing
about `onelake_security.apply` if `apply` stops calling it -- every assertion
against the helper would still pass while the view name went back to being
spliced raw. So the second half of this file drives `apply`, `restore`,
`_sweep_qualified`, `_drop`, `schemas_holding`, `secure_view_sql` and
`usercontext.register` with a recording fake engine and pins the EMITTED SQL
TEXT. A one-sided correction -- helper kept, call site reverted, or the reverse
-- fails here.

AND WHY A TABLE RATHER THAN A FEW CASES. The two tls_scope defects were a
missing IPv6 row and a prefix-vs-name row: both were single rows nobody thought
to write, and a table is what makes the omission visible. The rows below are
the ways a name can carry the quote character, not the ways someone imagined it
might.

THE BYTE-IDENTICAL ROW IS LOAD-BEARING TOO. `quote_ident("sales")` must be
exactly `` `sales` ``, because that is what lets this repair be applied at
every site in the agent without moving one fixture, recording or e2e output --
and the diff of this change is reviewable only because that is true.
"""
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))
import onelake_security as ols  # noqa: E402
import usercontext  # noqa: E402
from sqlident import quote_ident, quote_literal  # noqa: E402

# name -> the one correct quoting of it. Every backtick doubled, the whole
# wrapped in a fresh pair.
IDENTIFIERS = [
    # The ordinary case, and the reason the repair moved no output: this is
    # byte-for-byte the hand-written f"`{name}`" it replaced.
    ("sales", "`sales`"),
    ("silver_customers", "`silver_customers`"),
    # A DOTTED name is quoted WHOLE, not split. `quote_ident("a.b")` is one
    # identifier literally called "a.b" -- callers that want a qualified name
    # join two calls with a dot (`catalog.register` does), so a dot arriving
    # inside a single segment is part of the name and must not become a
    # qualification the caller did not write.
    ("a.b", "`a.b`"),
    ("lake.sales", "`lake.sales`"),
    # ONE backtick: the minimal escape.
    ("we`ird", "`we``ird`"),
    ("`", "````"),
    # A RUN of them, which is where a replace-once or a strip would differ
    # from a doubling.
    ("a``b", "`a````b`"),
    ("```", "````````"),
    # THE BREAKOUT THIS RULE EXISTS FOR, in the shape that matters: a name that
    # closes its own quoting and appends a clause. Doubled, the whole thing
    # stays one identifier and the payload is inert text inside it.
    ("x` AS SELECT * FROM secret --", "`x`` AS SELECT * FROM secret --`"),
    ("t` AS SELECT 1; DROP TABLE sales --",
     "`t`` AS SELECT 1; DROP TABLE sales --`"),
    # Empty: passed through as an empty quoted identifier rather than refused.
    # It is a bug upstream, not an escape, and the engine rejects it with a
    # message naming the statement -- more useful than a ValueError from here.
    ("", "``"),
    # Not a string. Names reach these helpers from JSON and from engine rows,
    # so the contract is str() and then escape, never "assume a str".
    (7, "`7`"),
]

# NUL and a line break cannot appear in a real identifier and can truncate or
# split a statement depending on who parses it, so they FAIL CLOSED.
REFUSED = ["a\x00b", "\x00", "a\nb", "a\r\nb", "sales\n-- ", "a\rb"]


@pytest.mark.parametrize("name,want", IDENTIFIERS)
def test_an_identifier_is_quoted_with_its_backticks_doubled(name, want):
    assert quote_ident(name) == want


@pytest.mark.parametrize("name", REFUSED)
def test_a_name_carrying_a_nul_or_a_line_break_is_refused(name):
    with pytest.raises(ValueError):
        quote_ident(name)


def test_quoting_is_idempotent_in_the_sense_that_matters():
    """Quoting twice nests rather than cancelling.

    The property worth pinning is that a second pass cannot UNDO the first --
    an escape that round-tripped would mean two calls on the same path (easy to
    introduce, hard to see) silently reopened the hole.
    """
    once = quote_ident("we`ird")
    assert quote_ident(once) == "```we````ird```"
    assert once in quote_ident(once).replace("``", "`")


@pytest.mark.parametrize("value,want", [
    ("abfss://lake@onelake/Tables/sales", "'abfss://lake@onelake/Tables/sales'"),
    ("plain", "'plain'"),
    ("", "''"),
    # The literal half of the mirror: `'` doubled, exactly as internal/tds's
    # `quotedID` and internal/server's `sqlLiteral` do it.
    ("it's", "'it''s'"),
    ("a' ; DROP TABLE t --", "'a'' ; DROP TABLE t --'"),
    ("''", "''''''"),
])
def test_a_literal_is_quoted_with_its_apostrophes_doubled(value, want):
    assert quote_literal(value) == want


@pytest.mark.parametrize("value", REFUSED)
def test_a_literal_carrying_a_nul_or_a_line_break_is_refused(value):
    with pytest.raises(ValueError):
        quote_literal(value)


# --- the real call paths ----------------------------------------------------
# Everything above is about the helper. Everything below is about whether the
# agent still uses it, which is the half a correct helper cannot tell you.

BREAKOUT = "x` AS SELECT * FROM secret --"
QUOTED = "`x`` AS SELECT * FROM secret --`"


class RecordingSpark:
    """Records statements and answers the two catalog probes `apply` makes."""

    def __init__(self, catalog=None, temp=(), fail_on=()):
        self.ran = []
        self.catalog = catalog or {}
        self.temp = set(temp)
        self.fail_on = fail_on

    def sql(self, q):
        self.ran.append(q)
        if any(f in q for f in self.fail_on):
            raise RuntimeError("boom")
        if q == "SHOW DATABASES":
            return _Rows([(db,) for db in self.catalog])
        if q.startswith("SHOW TABLES IN "):
            # The db arrives QUOTED, so unwrapping it here is part of the
            # assertion: a caller that stopped quoting would miss every table
            # and the sweep below would silently find nothing to drop.
            db = q[len("SHOW TABLES IN "):].strip()
            assert db.startswith("`") and db.endswith("`"), q
            name = db[1:-1].replace("``", "`")
            return _Rows([(name, t, t in self.temp)
                          for t in self.catalog.get(name, [])])
        return _Rows([])


class _Rows:
    def __init__(self, rows):
        self._rows = rows

    def collect(self):
        return self._rows


def test_the_view_that_enforces_the_policy_quotes_its_own_name():
    """`apply`'s CREATE OR REPLACE TEMP VIEW, pinned as text.

    THE SITE THIS WHOLE RULE WAS WRITTEN FOR. Before the repair this emitted
    `CREATE OR REPLACE TEMP VIEW x` AS SELECT * FROM secret -- AS SELECT ...`,
    so a table whose OneLake folder carried a backtick chose the body of the
    view that decides which rows a principal sees.
    """
    spark = RecordingSpark()
    ols.apply(spark, {BREAKOUT: {"rows": "SELECT * FROM t WHERE r = 1",
                                 "columns": []}}, [BREAKOUT])
    views = [q for q in spark.ran if q.startswith("CREATE OR REPLACE TEMP VIEW")]
    assert views == [
        f"CREATE OR REPLACE TEMP VIEW {QUOTED} "
        "AS SELECT * FROM (SELECT * FROM t WHERE r = 1)"], spark.ran


def test_the_row_filter_is_still_spliced_unescaped():
    """The deliberate asymmetry, asserted so it cannot be "fixed" by mistake.

    A row filter IS the policy author's SQL and using it as the view's SOURCE
    is how this module avoids parsing user SQL. Quoting it would turn the
    filter into a table name and break every narrowed table, so the ledger
    entry for `apply` records the splice and this pins it.
    """
    spark = RecordingSpark()
    rows = "SELECT * FROM sales WHERE region_id = 1"
    ols.apply(spark, {"sales": {"rows": rows, "columns": []}}, ["sales"])
    assert any(f"AS SELECT * FROM ({rows})" in q for q in spark.ran), spark.ran


def test_a_column_only_grant_quotes_the_columns_and_the_table():
    # secure_view_sql is the site R8 cannot see -- it returns a string rather
    # than calling .sql() -- so its text is pinned directly.
    assert ols.secure_view_sql(BREAKOUT, {"rows": "", "columns": ["a`b", "c"]}) \
        == f"SELECT `a``b`, `c` FROM {QUOTED}"


def test_the_qualified_sweep_quotes_both_halves():
    """The sweep is enforcement: a secured table must lose every spelling.

    Measured in e2e/onelake-security-bypass: with the view in place but the
    qualified registration left behind, `default.sales` returned all three rows
    and both columns. So a sweep that fails to NAME the table is a bypass, and
    an unquoted backtick made the DROP statement unparseable -- which the
    `except` swallows, leaving the door open and reporting nothing.
    """
    spark = RecordingSpark(catalog={"default": [BREAKOUT], "lake": [BREAKOUT]})
    hit = ols._sweep_qualified(spark, BREAKOUT)
    assert hit == ["default", "lake"]
    drops = [q for q in spark.ran if q.startswith("DROP TABLE")]
    assert drops == [f"DROP TABLE IF EXISTS `default`.{QUOTED}",
                     f"DROP TABLE IF EXISTS `lake`.{QUOTED}"], spark.ran


def test_withholding_a_denied_table_quotes_it_in_both_statements():
    # `_drop` is the fail-closed path, and the other site R8 cannot see: its
    # templates are bound to `stmt` before `spark.sql(stmt)` reads them.
    spark = RecordingSpark()
    ols._drop(spark, BREAKOUT)
    assert [q for q in spark.ran if q.startswith("DROP")] == [
        f"DROP VIEW IF EXISTS {QUOTED}",
        f"DROP TABLE IF EXISTS {QUOTED}"], spark.ran


def test_restore_quotes_the_name_and_the_location():
    spark = RecordingSpark()
    assert ols.restore(spark, BREAKOUT, lambda _n: "abfss://l/it's/sales") is True
    assert spark.ran == [
        f"DROP VIEW IF EXISTS {QUOTED}",
        f"CREATE TABLE IF NOT EXISTS {QUOTED} "
        "USING delta LOCATION 'abfss://l/it''s/sales'"], spark.ran


def test_schemas_holding_quotes_the_schema_it_probes():
    spark = RecordingSpark(catalog={"we`ird": ["sales"]})
    assert ols.schemas_holding(spark, "sales") == ["we`ird"]
    assert "SHOW TABLES IN `we``ird`" in spark.ran, spark.ran


def test_the_user_context_quotes_the_names_it_binds():
    """`usercontext.register`, the two-context path's half of the same work.

    Separate from `apply` on purpose: these are two different enforcement
    routes (reshape the catalog, or run the cell in a child with only permitted
    relations bound), and a repair applied to one is invisible in the other.
    """
    spark = RecordingSpark()
    g = {"spark": spark}
    out = usercontext.register(g, [{"name": BREAKOUT, "location": "abfss://l/t"}])
    assert out["status"] == "ok", out
    assert [q for q in spark.ran if "TEMP VIEW" in q] == [
        f"CREATE OR REPLACE TEMP VIEW {QUOTED} AS "
        "SELECT * FROM delta.`abfss://l/t`"], spark.ran

    # ...and the revocation half: a table the parent no longer lists must stop
    # being nameable, which needs the DROP to name it.
    out = usercontext.register(g, [])
    assert out["status"] == "ok", out
    assert f"DROP VIEW IF EXISTS {QUOTED}" in spark.ran, spark.ran


def test_an_ordinary_name_emits_exactly_what_it_did_before():
    """The no-drift guarantee, over the real paths rather than the helper.

    This is what makes the change reviewable: every statement below is
    character-for-character what the hand-written backticks produced, so no
    fixture, recording or e2e output moves.
    """
    spark = RecordingSpark(catalog={"default": ["sales"]})
    ols.apply(spark, {"sales": {"rows": "SELECT * FROM sales WHERE r = 1",
                                "columns": []}}, ["sales"],
              location_of=lambda _n: "abfss://lake/Tables/sales")
    assert "CREATE OR REPLACE TEMP VIEW `sales` AS " \
           "SELECT * FROM (SELECT * FROM sales WHERE r = 1)" in spark.ran, spark.ran
    assert "DROP TABLE IF EXISTS `default`.`sales`" in spark.ran, spark.ran
