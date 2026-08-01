"""Compile the ODCS quality rules in contracts/ into Great Expectations, and run
them against the data that actually landed.

Until this existed, the four contracts were documentation: they stated
`duplicateValues mustBe 0` and nothing ever checked it. A contract nobody
executes is a comment with a schema, and it rots exactly like a comment does —
the `rowCount mustBe: 7` pin in silver-sales survived a hundred-thousand-fold
change of scale without anyone noticing, because nothing read it.

**Why compile rather than hand-roll.** ODCS's `library` metrics are a portable
vocabulary deliberately defined without reference to any engine, and Great
Expectations is an engine with a matching vocabulary. Translating one into the
other is the honest way to execute a library rule — it keeps the contract
engine-independent, which is the property that makes `library` worth preferring
over `sql` in the first place (see docs/30). The mapping:

    nullValues      mustBe 0        -> expect_column_values_to_not_be_null
    duplicateValues mustBe 0        -> expect_column_values_to_be_unique
                                       expect_compound_columns_to_be_unique
    invalidValues   + validValues   -> expect_column_values_to_be_in_set
    rowCount        mustBe          -> expect_table_row_count_to_equal
    rowCount        mustBeGreaterThan -> expect_table_row_count_to_be_between
    columnCount     mustBe          -> expect_table_column_count_to_equal

**What this does NOT execute.** ODCS's library has exactly five metrics, and
referential integrity is not among them, so cross-table rules live in the
contracts as `type: sql` carrying T-SQL. Those need a warehouse, not a
DataFrame, and this runner does not have one. It counts them and says so out
loud rather than skipping them silently — a gate that quietly ignores a class of
rule reports green on exactly the rule you most recently wrote.
"""
import itertools
import os
import pathlib

# Before importing GX: it renders a tqdm progress bar per metric, which on a
# 100,000-row frame emits hundreds of carriage-returned lines and buries the
# actual pass/fail output. setdefault, so a caller who wants them can ask.
os.environ.setdefault("TQDM_DISABLE", "1")

import great_expectations as gx  # noqa: E402 — must follow TQDM_DISABLE
import yaml  # noqa: E402

HERE = pathlib.Path(__file__).resolve().parent
CONTRACTS = HERE / "contracts"
_names = itertools.count()


class ContractViolation(AssertionError):
    """Raised when data does not satisfy its contract. An assertion, not a
    warning: a DQ gate that returns a report nobody reads is not a gate."""


def load(name):
    """Load a contract by file stem, e.g. 'silver-sales'."""
    with (CONTRACTS / f"{name}.odcs.yaml").open() as f:
        return yaml.safe_load(f)


def _entry(contract, element):
    for s in contract.get("schema", []):
        if s["name"] == element:
            return s
    raise KeyError(f"{contract['name']}: no schema entry named {element!r}")


def _validator(df):
    ctx = gx.get_context()
    n = next(_names)
    src = ctx.sources.add_pandas(f"src{n}")
    asset = src.add_dataframe_asset(f"asset{n}")
    br = asset.build_batch_request(dataframe=df)
    return ctx.get_validator(batch_request=br,
                             create_expectation_suite_with_name=f"suite{n}")


def _apply(v, rule, df, column, out, unexecuted):
    """Translate one ODCS rule into an expectation and record the outcome.

    `out` collects failures; `unexecuted` collects rules this engine cannot run.
    """
    rtype = rule.get("type", "library")
    if rtype == "sql":
        unexecuted.append(rule.get("description") or "sql rule")
        return 0
    if rtype != "library":
        out.append(f"unsupported rule type {rtype!r} — contract_gate must learn it")
        return 0

    metric = rule.get("metric")
    args = rule.get("arguments") or {}

    if metric == "rowCount":
        if "mustBe" in rule:
            r = v.expect_table_row_count_to_equal(value=rule["mustBe"])
            if not r.success:
                out.append(f"rowCount: expected {rule['mustBe']:,}, got {len(df):,}")
        if "mustBeGreaterThan" in rule:
            r = v.expect_table_row_count_to_be_between(min_value=rule["mustBeGreaterThan"] + 1)
            if not r.success:
                out.append(f"rowCount: expected > {rule['mustBeGreaterThan']:,}, got {len(df):,}")
        if "mustBeLessThan" in rule:
            r = v.expect_table_row_count_to_be_between(max_value=rule["mustBeLessThan"] - 1)
            if not r.success:
                out.append(f"rowCount: expected < {rule['mustBeLessThan']:,}, got {len(df):,}")

    elif metric == "columnCount":
        r = v.expect_table_column_count_to_equal(value=rule["mustBe"])
        if not r.success:
            out.append(f"columnCount: expected {rule['mustBe']}, got {len(df.columns)}")

    elif metric == "duplicateValues":
        cols = args.get("columns") or ([column] if column else None)
        if not cols:
            out.append("duplicateValues: no column to check")
            return 1
        missing = [c for c in cols if c not in df.columns]
        if missing:
            out.append(f"duplicateValues: absent column(s) {missing}")
            return 1
        if len(cols) == 1:
            r = v.expect_column_values_to_be_unique(cols[0])
        else:
            r = v.expect_compound_columns_to_be_unique(column_list=cols)
        if not r.success:
            n = int(df.duplicated(subset=cols).sum())
            out.append(f"duplicateValues{cols}: expected 0, got {n:,}")

    elif metric == "invalidValues":
        valid = args.get("validValues")
        if valid is None or column is None:
            out.append("invalidValues: needs validValues and a column")
            return 1
        if column not in df.columns:
            out.append(f"invalidValues: absent column {column!r}")
            return 1
        r = v.expect_column_values_to_be_in_set(column, value_set=valid)
        if not r.success:
            bad = sorted(set(df.loc[~df[column].isin(valid), column].head(5)))
            n = int((~df[column].isin(valid)).sum())
            out.append(f"invalidValues[{column}]: expected 0, got {n:,} (e.g. {bad})")

    elif metric in ("nullValues", "missingValues"):
        if column is None or column not in df.columns:
            out.append(f"{metric}: absent column {column!r}")
            return 1
        r = v.expect_column_values_to_not_be_null(column)
        if not r.success:
            out.append(f"{metric}[{column}]: expected 0, got {int(df[column].isna().sum()):,}")

    else:
        # Deliberately fatal. A gate that skips what it does not understand
        # reports green on the rule you most recently added.
        out.append(f"UNSUPPORTED metric {metric!r} — contract_gate must learn it")
    return 1


def validate_sql(conn, contract_name, element, verbose=True):
    """Execute a contract's `type: sql` rules against a live warehouse.

    ODCS's library has exactly five metrics and referential integrity is not
    among them, so the single most common star-schema guarantee — "every fact
    row resolves to a dimension row" — can only be written as SQL (docs/30).
    Those rules need a query engine, not a DataFrame, which is why they run
    here over TDS rather than through Great Expectations.

    `{object}` is substituted with the element's physical name, the only thing
    keeping these queries from hard-coding table names.
    """
    entry = _entry(load(contract_name), element)
    obj = entry.get("physicalName") or entry["name"]
    failures, checked = [], 0

    def run(rule):
        nonlocal checked
        if rule.get("type") != "sql":
            return
        checked += 1
        query = rule["query"].replace("{object}", obj).replace("{property}", "")
        got = conn.cursor().execute(query).fetchone()[0]
        want = rule.get("mustBe", 0)
        if got != want:
            failures.append(f"{rule.get('name', 'sql')}: expected {want}, got {got:,}"
                            f" — {rule.get('description', '')}".rstrip(" —"))

    for rule in entry.get("quality", []) or []:
        run(rule)
    for prop in entry.get("properties", []) or []:
        for rule in prop.get("quality", []) or []:
            run(rule)

    if failures:
        raise ContractViolation(
            f"{contract_name} / {element}: {len(failures)} sql rule(s) violated\n  - "
            + "\n  - ".join(failures))
    if verbose and checked:
        print(f"==> contract {contract_name}/{element}: {checked} sql rule(s) satisfied "
              f"against the warehouse", flush=True)
    return checked


def validate(df, contract_name, element, verbose=True):
    """Check `df` against one schema element of one contract.

    Returns the number of rules executed; raises ContractViolation on failure.
    """
    entry = _entry(load(contract_name), element)
    v = _validator(df)
    failures, unexecuted, checked = [], [], 0

    for rule in entry.get("quality", []) or []:
        checked += _apply(v, rule, df, None, failures, unexecuted)

    for prop in entry.get("properties", []) or []:
        col = prop["name"]
        # `required` and `unique` are shorthand for rules ODCS also lets you
        # spell out; honour both spellings so a contract can use either.
        if prop.get("required") and col in df.columns:
            checked += 1
            if not v.expect_column_values_to_not_be_null(col).success:
                failures.append(f"required[{col}]: {int(df[col].isna().sum()):,} null(s)")
        if prop.get("unique") and col in df.columns:
            checked += 1
            if not v.expect_column_values_to_be_unique(col).success:
                failures.append(f"unique[{col}]: {int(df.duplicated(subset=[col]).sum()):,} dup(s)")
        for rule in prop.get("quality", []) or []:
            checked += _apply(v, rule, df, col, failures, unexecuted)

    if failures:
        raise ContractViolation(
            f"{contract_name} / {element}: {len(failures)} rule(s) violated\n  - "
            + "\n  - ".join(failures))
    if verbose:
        note = f", {len(unexecuted)} sql rule(s) NOT executed (need a warehouse)" \
            if unexecuted else ""
        print(f"==> contract {contract_name}/{element}: {checked} rule(s) satisfied "
              f"over {len(df):,} rows via Great Expectations{note}", flush=True)
    return checked
