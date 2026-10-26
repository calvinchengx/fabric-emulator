"""The gold ODCS contract and the dbt tests that enforce it must agree (O3).

docs/30-odcs-data-contracts.md chose to keep both artifacts hand-authored and
let CI fail when they diverge. This is that failure: every quality rule in
`gold-sales.odcs.yaml` must have a dbt test behind it, and every dbt test must
be promised by the contract. Silver and landing are out of scope on purpose:
their rules are fixture assertions run by the pipeline, not dbt tests.

Rules are reduced to enforcer tuples, one vocabulary for both sides:

    ("not_null", model, column)
    ("unique", model, column)
    ("accepted_values", model, column, (values...))
    ("relationships", model, column, parent_model)
    ("singular", model, normalised_where_clause)
"""

import copy
import pathlib
import re

import pytest
import yaml

GOLD = pathlib.Path(__file__).resolve().parents[2] / "examples" / "medallion-pyspark"
CONTRACT = GOLD / "contracts" / "gold-sales.odcs.yaml"
SCHEMA_YML = GOLD / "gold" / "models" / "schema.yml"
SINGULAR_DIR = GOLD / "gold" / "tests"

RELATIONSHIPS_RE = re.compile(r"dbt `relationships` to ref\('(\w+)'\)")
SINGULAR_RE = re.compile(r"gold/tests/(\w+\.sql)")
WHERE_RE = re.compile(r"\bwhere\b(.*)$", re.IGNORECASE | re.DOTALL)


def _where(sql: str) -> str:
    """The WHERE clause, whitespace- and case-folded, `;` and jinja stripped."""
    m = WHERE_RE.search(sql)
    assert m, f"no WHERE clause in {sql!r}"
    return " ".join(m.group(1).replace(";", " ").split()).lower()


def _contract_rule(model: str, column: str | None, rule: dict, singular: dict[str, str]):
    """Reduce one ODCS rule to an enforcer tuple, or fail on one we can't read."""
    kind = rule["type"]
    where = f"{model}.{column}" if column else model
    if kind == "library":
        metric = rule["metric"]
        assert rule.get("mustBe") == 0, f"{where}: {metric} must be pinned to 0"
        assert column, f"{where}: library metric {metric} needs a column"
        if metric == "nullValues":
            return ("not_null", model, column)
        if metric == "duplicateValues":
            return ("unique", model, column)
        if metric == "invalidValues":
            values = tuple(rule["arguments"]["validValues"])
            return ("accepted_values", model, column, values)
        pytest.fail(f"{where}: no dbt enforcer is known for metric {metric}")
    assert kind == "sql", f"{where}: unsupported rule type {kind}"
    assert rule.get("mustBe") == 0, f"{where}: sql rule must be pinned to 0"
    desc = " ".join(rule["description"].split())
    rel = RELATIONSHIPS_RE.search(desc)
    if rel:
        assert column, f"{where}: relationships rule needs a column"
        return ("relationships", model, column, rel.group(1))
    single = SINGULAR_RE.search(desc)
    assert single, f"{where}: sql rule names neither a dbt relationships nor a gold/tests file"
    name = single.group(1)
    assert name in singular, f"{where}: {name} is not under gold/tests"
    return ("singular", model, _where(rule["query"]))


def contract_enforcers(contract: dict, singular: dict[str, str]) -> set[tuple]:
    out: set[tuple] = set()
    for obj in contract["schema"]:
        model = obj["name"]
        for rule in obj.get("quality", []):
            out.add(_contract_rule(model, None, rule, singular))
        for prop in obj.get("properties", []):
            for rule in prop.get("quality", []):
                out.add(_contract_rule(model, prop["name"], rule, singular))
    return out


def dbt_enforcers(schema: dict, singular: dict[str, str]) -> set[tuple]:
    out: set[tuple] = set()
    for model in schema["models"]:
        for col in model.get("columns", []):
            for test in col.get("tests", []):
                if isinstance(test, str):
                    out.add((test, model["name"], col["name"]))
                    continue
                ((name, args),) = test.items()
                if name == "accepted_values":
                    out.add((name, model["name"], col["name"], tuple(args["values"])))
                elif name == "relationships":
                    parent = re.fullmatch(r"ref\('(\w+)'\)", args["to"])
                    assert parent, f"unreadable relationships target {args['to']!r}"
                    out.add((name, model["name"], col["name"], parent.group(1)))
                else:
                    pytest.fail(f"{model['name']}.{col['name']}: unknown dbt test {name}")
    # A singular test is attributed to the model it selects from.
    for sql in singular.values():
        ref = re.search(r"ref\('(\w+)'\)", sql)
        assert ref, "singular test selects from no ref()"
        out.add(("singular", ref.group(1), _where(sql)))
    return out


def _load():
    singular = {p.name: p.read_text() for p in sorted(SINGULAR_DIR.glob("*.sql"))}
    contract = yaml.safe_load(CONTRACT.read_text())
    schema = yaml.safe_load(SCHEMA_YML.read_text())
    return contract, schema, singular


def _diff(contract, schema, singular):
    declared = contract_enforcers(contract, singular)
    enforced = dbt_enforcers(schema, singular)
    return declared - enforced, enforced - declared


def test_every_contract_rule_has_a_dbt_test_and_every_dbt_test_is_promised():
    contract, schema, singular = _load()
    unenforced, unpromised = _diff(contract, schema, singular)
    assert not unenforced, f"contract rules with no dbt enforcer: {sorted(unenforced)}"
    assert not unpromised, f"dbt tests the contract never promised: {sorted(unpromised)}"


def test_the_comparison_sees_the_rules_it_is_meant_to_compare():
    # Guards against a vacuous pass: an empty-equals-empty diff is green too.
    contract, _, singular = _load()
    declared = contract_enforcers(contract, singular)
    kinds = {t[0] for t in declared}
    assert kinds == {"not_null", "unique", "accepted_values", "relationships", "singular"}
    assert len(declared) >= 10


def test_dropping_a_dbt_test_is_reported_as_an_unenforced_rule():
    contract, schema, singular = _load()
    schema = copy.deepcopy(schema)
    fct = next(m for m in schema["models"] if m["name"] == "fct_orders")
    amount = next(c for c in fct["columns"] if c["name"] == "amount")
    amount["tests"] = []
    unenforced, unpromised = _diff(contract, schema, singular)
    assert unenforced == {("not_null", "fct_orders", "amount")}
    assert not unpromised


def test_a_dbt_test_the_contract_never_declared_is_reported_as_unpromised():
    contract, schema, singular = _load()
    schema = copy.deepcopy(schema)
    dim = next(m for m in schema["models"] if m["name"] == "dim_customer")
    dim["columns"].append({"name": "name", "tests": ["not_null"]})
    unenforced, unpromised = _diff(contract, schema, singular)
    assert unpromised == {("not_null", "dim_customer", "name")}
    assert not unenforced


def test_widening_accepted_values_on_one_side_only_is_drift():
    contract, schema, singular = _load()
    schema = copy.deepcopy(schema)
    dim = next(m for m in schema["models"] if m["name"] == "dim_customer")
    country = next(c for c in dim["columns"] if c["name"] == "country")
    for t in country["tests"]:
        if isinstance(t, dict):
            t["accepted_values"]["values"].append("FR")
    unenforced, unpromised = _diff(contract, schema, singular)
    assert {t[3] for t in unenforced} == {("US", "GB", "SG")}
    assert {t[3] for t in unpromised} == {("US", "GB", "SG", "FR")}


def test_a_singular_test_that_diverges_from_the_sql_rule_is_drift():
    contract, schema, singular = _load()
    singular = {k: v.replace("revenue <= 0", "revenue < 0") for k, v in singular.items()}
    unenforced, unpromised = _diff(contract, schema, singular)
    assert [t[0] for t in unenforced] == ["singular"]
    assert [t[0] for t in unpromised] == ["singular"]


def test_an_unknown_library_metric_fails_loudly_rather_than_being_skipped():
    contract, _, singular = _load()
    contract = copy.deepcopy(contract)
    contract["schema"][0]["properties"][0].setdefault("quality", []).append(
        {"type": "library", "metric": "rowCount", "mustBe": 0}
    )
    with pytest.raises(pytest.fail.Exception, match="no dbt enforcer is known for metric rowCount"):
        contract_enforcers(contract, singular)
