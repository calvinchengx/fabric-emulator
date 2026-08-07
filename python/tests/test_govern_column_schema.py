"""The payloads `govern_ingest` sends are checked against OpenMetadata's OWN schema.

Nothing validated them before. `scripts/check_govern_types.py` checks one rule,
hand-transcribed from a 400 this repo already paid for; everything else about a
column — the `dataType` enum, required fields, field types, `children` nesting —
was unchecked until the containerized `ci:governance` e2e ran, and that suite
needs postgres, opensearch and the OpenMetadata server to start at all. A typo
in `TYPE_MAP` cost a full container run to discover.

THE VALIDATED NODE IS `column`, NOT `table`. That is the whole reason this is
cheap. `table.json#/definitions/column` reaches three external schemas whose
closure is eight files; validating `table.json` itself would pull 146 files and
451 KB, because it references `databaseService.json` and therefore every
connector configuration OpenMetadata supports. See
third_party/openmetadata-schema/PROVENANCE.md.

WHAT THIS CANNOT CATCH, stated here so the next reader does not delete
check_govern_types.py in favour of it: `dataLength` is required for
char/varchar/binary/varbinary, and that rule is NOT in the schema. `column`
declares only `required: ["name", "dataType"]`; the constraint lives in prose in
a description and is enforced in OpenMetadata's Java layer. A validator passes
the exact payload that costs the whole table.
"""
import hashlib
import json
import pathlib
import re
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

from check_govern_types import load_ingest  # noqa: E402

jsonschema = pytest.importorskip("jsonschema")
from referencing import Registry, Resource  # noqa: E402
from referencing.jsonschema import DRAFT7  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parents[2]
VENDOR = ROOT / "third_party" / "openmetadata-schema"
SCHEMA_DIR = VENDOR / "schema"
COLUMN = "file:///entity/data/table.json#/definitions/column"


@pytest.fixture(scope="module")
def validator():
    """A Draft-07 validator for one column, resolving $refs from the vendored copy.

    Every vendored file is registered under a `file:///` URI mirroring its path,
    so the relative `$ref`s upstream writes (`../../type/basic.json`) resolve
    without rewriting a byte of the schema — the sha256 in PROVENANCE.md is only
    evidence if what we load is what we recorded.
    """
    resources = {}
    for p in SCHEMA_DIR.rglob("*.json"):
        uri = "file:///" + p.relative_to(SCHEMA_DIR).as_posix()
        resources[uri] = Resource(contents=json.loads(p.read_text()), specification=DRAFT7)
    registry = Registry().with_resources(resources.items())
    return jsonschema.Draft7Validator({"$ref": COLUMN}, registry=registry)


@pytest.fixture(scope="module")
def gi():
    return load_ingest()


def errors(validator, column):
    return [e.message for e in validator.iter_errors(column)]


# --- every column the mapper can produce --------------------------------------

def test_every_type_map_entry_produces_a_valid_column(validator, gi):
    # The whole map, not a sample: a target that OpenMetadata does not know is a
    # 400 on the entire table, and the map is where a wrong name would live.
    for delta_type in gi.TYPE_MAP:
        col = gi.om_column(f"c_{delta_type}", delta_type)
        assert not errors(validator, col), f"{delta_type} -> {col}"


def test_parameterised_decimal_is_valid(validator, gi):
    # delta-rs renders these as "decimal(10,2)"; precision and scale are the
    # detail a catalog is judged on, so they must survive schema validation.
    col = gi.om_column("amount", "decimal(10,2)")
    assert not errors(validator, col), col
    assert col["dataType"] == "DECIMAL"


def test_nested_struct_children_are_valid(validator, gi):
    # `children` recurses into the same definition, so a malformed child is
    # exactly the shape a flat check would miss.
    col = gi.om_column("addr", {"type": "struct", "fields": [
        {"name": "city", "type": "string", "nullable": True},
        {"name": "zip", "type": "long", "nullable": False},
    ]})
    assert not errors(validator, col), col
    assert [c["name"] for c in col["children"]] == ["city", "zip"]


def test_array_element_type_is_valid(validator, gi):
    col = gi.om_column("tags", {"type": "array", "elementType": "string"})
    assert not errors(validator, col), col


# --- the schema actually refuses things ---------------------------------------
#
# A validator that accepts everything passes every test above. These pin that it
# does not.

@pytest.mark.parametrize("bad,why", [
    ({"name": "c", "dataType": "BIGINTT"}, "is not one of"),          # typo'd enum
    ({"dataType": "BIGINT"}, "required"),                              # no name
    ({"name": "c"}, "required"),                                       # no dataType
    ({"name": "c", "dataType": "VARCHAR", "dataLength": "20"}, "is not of type"),
    ({"name": "c", "dataType": "STRUCT",
      "children": [{"dataType": "INT"}]}, "required"),                 # child has no name
])
def test_the_schema_refuses_malformed_columns(validator, bad, why):
    msgs = errors(validator, bad)
    assert msgs, f"schema accepted {bad}"
    assert any(why in m for m in msgs), msgs


def test_a_type_map_target_outside_the_enum_would_fail(validator, gi):
    # The mutation the issue asks for, run as a test rather than by hand: point
    # one mapping at a name OpenMetadata does not know and the column must be
    # refused. If this passes, the enum is not really being checked.
    col = gi.om_column("c", "string")
    col["dataType"] = "STRINGG"
    assert errors(validator, col), "a dataType outside the enum was accepted"


# --- and the thing it cannot see, asserted rather than described --------------

def test_the_schema_accepts_the_column_that_costs_the_whole_table(validator, gi):
    """DO NOT DELETE check_govern_types.py IN FAVOUR OF THIS FILE.

    The header says the `dataLength` rule is not in the schema. This proves it,
    so the claim cannot quietly stop being true: a BINARY column with no
    `dataLength` — the exact payload OpenMetadata answers with 400 for the
    ENTIRE table — validates clean here.

    WHEN THIS FIRES, precisely — it does not watch upstream. It validates the
    VENDORED 1.13.2 bytes, which change only when someone re-vendors, so an
    OpenMetadata release that encodes the rule does not trip it on its own.
    What closes the chain is the pin: `test_the_pinned_schema_matches_the_
    version_docker_compose_runs` fails the moment the compose image is bumped
    without re-vendoring, and re-vendoring is what brings new bytes here. So the
    sequence is image bump -> pin test fails -> re-vendor -> this test fails if
    the rule arrived. Do not read it as drift detection against upstream; it
    cannot carry that.

    If it does fail after a re-vendor, the schema has grown the rule (an
    `if`/`then`, an `allOf`, a `dependentRequired`) and the cheap guard's job
    has changed. Revisit both, do not delete either on the strength of one.
    """
    assert errors(validator, {"name": "c", "dataType": "BINARY"}) == []

    column = json.loads((SCHEMA_DIR / "entity/data/table.json").read_text())
    column = column["definitions"]["column"]
    assert column["required"] == ["name", "dataType"]
    assert not any(k in column for k in ("if", "then", "allOf", "dependentRequired"))

    # And the guard that DOES catch it still does, on the same input.
    assert gi.om_column("b", "binary")["dataLength"] == gi.DEFAULT_BINARY_LENGTH


# --- the vendored copy must be what we recorded, and what we run --------------

def test_vendored_files_match_their_recorded_hashes():
    # PROVENANCE.md's sha256 column is the tamper check third_party/README.md
    # requires. Unverified, it is decoration.
    provenance = (VENDOR / "PROVENANCE.md").read_text()
    recorded = dict(
        (m.group(1), m.group(3))
        for m in re.finditer(r"^\| `([^`]+)` \| (\d+) \| `([0-9a-f]{64})` \|$", provenance, re.M)
    )
    assert recorded, "PROVENANCE.md has no integrity table"
    on_disk = {
        p.relative_to(VENDOR).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
        for p in VENDOR.rglob("*")
        if p.is_file() and p.name != "PROVENANCE.md"
    }
    assert on_disk == recorded


def test_the_pinned_schema_matches_the_version_docker_compose_runs():
    # A schema validated against a version nobody runs is confidence, not
    # coverage. The compose file pins OpenMetadata in THREE places (postgres,
    # server, migration); all of them must agree with the vendored release, so
    # bumping the image without re-vendoring fails here rather than in a
    # container an hour later.
    provenance = (VENDOR / "PROVENANCE.md").read_text()
    m = re.search(r"tag `([0-9]+\.[0-9]+\.[0-9]+)-release`", provenance)
    assert m, "PROVENANCE.md does not record which release the pin is"
    vendored = m.group(1)

    compose = (ROOT / "docker-compose.yml").read_text()
    pins = set(re.findall(r"docker\.getcollate\.io/openmetadata/[a-z]+:([0-9.]+)", compose))
    assert pins, "no OpenMetadata image pins found in docker-compose.yml"
    assert pins == {vendored}, (
        f"vendored schema is {vendored} but docker-compose.yml pins {sorted(pins)} — "
        "re-vendor with scripts/vendor_openmetadata_schema.py or revert the image bump"
    )
