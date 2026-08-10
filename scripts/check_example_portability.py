#!/usr/bin/env python3
"""Every example must be target-portable, and persist artifacts the way Fabric does.

Two properties, both of which were false before this check existed and neither of
which prose was holding:

1. **The target is resolved in one place.** An example that hardcodes the
   emulator's seeded principal or a `localhost` endpoint is not portable, however
   loudly its README says otherwise — and the failure is invisible until someone
   sets `FABRIC_TARGET=real` and watches it authenticate against nothing. The
   four medallion examples were in exactly that state: `common.py` carried
   `daemon-app-secret` and `https://localhost:9443` defaults, so the toggle
   `docs/21` documents did not reach them at all.

2. **Definitions use Fabric's own part paths.** An item's source lives in
   definition parts whose paths are the CI/CD source format —
   `notebook-content.py`, `pipeline-content.json`, `.platform`
   (docs/46-artifact-persistence.md). An example that invents a path teaches a
   layout real Fabric will not accept, and nothing else in CI notices, because
   the emulator stores parts verbatim by design.

Usage:
    check_example_portability.py     exit non-zero on the first violation
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
EXAMPLES = ROOT / "examples"

# The resolver, and only the resolver, may name these. Everything else must ask it.
RESOLVER = "examples/contoso-fixtures/common.py"

# entra-emulator's seeded dev values. Hardcoding one pins an example to the
# emulator; `fabric-target` refuses them by value in real mode precisely because
# a leftover shell variable would otherwise reach production.
SEEDS = (
    "daemon-app-secret",
    "6f89cf12-978b-4d23-ac18-9ef0c127cf87",  # seeded tenant
    "00d88624-f0d7-46f6-a641-6232c2608928",  # seeded daemon SP
)

# A hardcoded FABRIC-SURFACE endpoint is the other half of the same problem. Only
# the surfaces the toggle actually resolves count: the control plane (9443), the
# token authority (8443) and the vault (8444). A companion service — OpenMetadata,
# a mock source API — is genuinely local policy with no real-Fabric counterpart to
# switch to, and flagging it would make this gate fire on correct code.
# contoso-data-platform's own equivalent check draws the line in the same place.
LOCALHOST = re.compile(r"https?://localhost:(9443|8443|8444)\b")

# The SQL endpoint is the same problem without a scheme, so the pattern above
# cannot see it. On real Fabric the address is per-item and assigned by the
# service (`properties.connectionString`, or `sqlEndpointProperties.connectionString`
# for a lakehouse's analytics endpoint), so a step naming a host has opted out of
# the toggle no matter how carefully it resolves everything else. Ask
# common.sql_endpoint() — see docs/46-artifact-persistence.md.
SQL_HOST = re.compile(r"localhost[,:]1433\b")

# Fabric's own definition part paths. Extend deliberately, with a doc reference:
# a path that is not here is either a typo or a contract this repo has not read.
KNOWN_PARTS = {
    ".platform",
    "notebook-content.py",
    "notebook-content.ipynb",
    "pipeline-content.json",
    "definition.pbism",
    "definition.pbir",
    "report.json",
    "definition/database.tmdl",
    "definition/model.tmdl",
    "definition/expressions.tmdl",
    "model.bim",
    "data.json",  # the emulator's own import-data part, documented in doc 46
    "Spark.json",
    "SparkJobDefinitionV1.json",
}

# A part path may also be a TMDL table file or a PBIP subpath, which are
# per-model names rather than a fixed list.
PART_SHAPES = (
    re.compile(r"^definition/tables/[^/]+\.tmdl$"),
    re.compile(r"^definition\.pbidataset$"),
    re.compile(r"^StaticResources/.+$"),
)

# Only paths that are DEFINITION parts, which is not every `"path"` key: the
# examples also carry shortcut targets ("Tables/bronze_orders"), source URLs, and
# PBIP directory names, none of which are item source. The tell for a real part
# is `payloadType` in the same object — that is what makes it a part rather than
# a path-shaped string. Getting this wrong made the first version of this check
# report 31 violations, none of them real.
PART_KEY = re.compile(r'"path"\s*:\s*"([^"]+)"[^}]{0,120}?payloadType', re.S)
# The examples' own helper: create_item(name, type, {"<part path>": body}).
PART_DICT = re.compile(r'create_item\(\s*[^,]+,\s*[^,]+,\s*\{\s*"([^"]+)"\s*:', re.S)


# Installed dependencies are not this repo's examples. A gate that reads
# site-packages reports other people's code and trains its readers to ignore it.
SKIP_DIRS = {"__pycache__", ".venv", "site-packages", "node_modules", "build"}


def python_files(root):
    return sorted(p for p in root.rglob("*.py")
                  if not SKIP_DIRS & set(p.parts))


def rel(p):
    return str(p.relative_to(ROOT))


def check_no_pinned_target(problems):
    """Only the resolver may name a seed or a localhost endpoint."""
    for f in python_files(EXAMPLES):
        if rel(f) == RESOLVER:
            continue
        text = f.read_text()
        for seed in SEEDS:
            if seed in text:
                problems.append(
                    f"{rel(f)} hardcodes the emulator's seeded credential ({seed[:18]}…). "
                    f"Ask the resolver instead — see {RESOLVER} and docs/21.")
        for m in LOCALHOST.finditer(text):
            problems.append(
                f"{rel(f)} hardcodes {m.group(0)}. An example that names an endpoint "
                f"cannot follow FABRIC_TARGET; resolve it through {RESOLVER}.")
        for m in SQL_HOST.finditer(text):
            problems.append(
                f"{rel(f)} hardcodes the SQL endpoint {m.group(0)}. On real Fabric "
                f"that address is per-item and only the API knows it — ask "
                f"{RESOLVER}'s sql_endpoint(), which discovers it on both targets "
                f"(docs/46-artifact-persistence.md).")


# A TLS bypass and an emulator-only control endpoint are the other two ways an
# example silently pins itself. Both were absent when this gate was written —
# absent by luck, since nothing checked. contoso-data-platform's equivalent
# already covers the TLS half, so matching it keeps the two repos honest about
# the same thing.
TLS_BYPASS = re.compile(r"verify\s*=\s*False|allow_invalid_certificates|azure_allow_http")
EMULATOR_CONTROL = re.compile(r"_emulator/(clock|faults|events)")


def check_no_emulator_only_leaks(problems):
    """A TLS bypass or a control-plane lever outside the resolver is a pin."""
    for f in python_files(EXAMPLES):
        if rel(f) == RESOLVER:
            continue  # the resolver is where local reality is allowed to exist
        for i, line in enumerate(f.read_text().splitlines(), 1):
            if line.lstrip().startswith("#"):
                continue
            if TLS_BYPASS.search(line):
                problems.append(
                    f"{rel(f)}:{i} bypasses TLS verification. Real Fabric serves real "
                    f"certificates; a bypass here ships to production invisibly. Put it "
                    f"in {RESOLVER}, behind the target.")
            if EMULATOR_CONTROL.search(line):
                problems.append(
                    f"{rel(f)}:{i} calls an emulator-only control endpoint "
                    f"({EMULATOR_CONTROL.search(line).group(0)}). Real Fabric has no such "
                    f"lever — guard it with the target's emulator_only()/clock capability, "
                    f"as contoso-data-platform's platform/schedule.py does.")


def check_definition_folders(problems):
    """Any `definitions/` directory must hold Fabric's source format.

    The layout is the artefact: `<display name>.<Type>/` with the definition
    files AND `.platform`. A folder missing `.platform` deploys fine here — the
    emulator stores whatever parts it is given — and is not what Fabric's Git
    integration produces, so the example would be teaching a shape no CI/CD tool
    round-trips. That asymmetry is the whole reason this is checked rather than
    described.
    """
    for defs in sorted(EXAMPLES.glob("*/definitions")):
        if not defs.is_dir():
            continue
        folders = [d for d in sorted(defs.iterdir()) if d.is_dir()]
        if not folders:
            problems.append(f"{rel(defs)} exists but holds no item folders.")
        for d in folders:
            name, _, item_type = d.name.rpartition(".")
            if not name or not item_type:
                problems.append(
                    f"{rel(d)} is not named '<display name>.<Type>' — that is the "
                    f"layout Fabric's Git integration writes (docs/46).")
                continue
            if not (d / ".platform").is_file():
                problems.append(
                    f"{rel(d)} has no .platform. Every item carries one; it holds the "
                    f"logicalId that survives renames (docs/46).")
            if not any(f.is_file() and f.name != ".platform" for f in d.iterdir()):
                problems.append(f"{rel(d)} has a .platform and no definition file.")


def check_resolver_uses_the_contract(problems):
    """The resolver must consume `fabric-target`, not restate it."""
    f = ROOT / RESOLVER
    if not f.exists():
        problems.append(f"{RESOLVER} is missing — the resolver is where the target is decided.")
        return
    text = f.read_text()
    if "fabric_target" not in text:
        problems.append(
            f"{RESOLVER} does not import fabric_target. Restating the contract is how "
            f"contoso-data-platform drifted into requiring a client secret, which broke "
            f"az login, managed identity, and running inside a Fabric notebook.")
    if "daemon-app-secret" in text:
        problems.append(
            f"{RESOLVER} still hardcodes the seeded secret; the resolver should take it "
            f"from fabric_target so real mode can refuse it by value.")


def known_part(path):
    return path in KNOWN_PARTS or any(s.match(path) for s in PART_SHAPES)


def check_definition_parts(problems):
    """Every definition part path must be one Fabric actually accepts."""
    for f in python_files(EXAMPLES):
        text = f.read_text()
        for pat in (PART_KEY, PART_DICT):
            for m in pat.finditer(text):
                path = m.group(1)
                if not known_part(path):
                    problems.append(
                        f"{rel(f)} writes a definition part {path!r}, which is not a "
                        f"Fabric source-format path. See docs/46-artifact-persistence.md; "
                        f"add it to KNOWN_PARTS only with a reference to the contract.")


def main():
    problems = []
    check_no_pinned_target(problems)
    check_no_emulator_only_leaks(problems)
    check_definition_folders(problems)
    check_resolver_uses_the_contract(problems)
    check_definition_parts(problems)

    examples = sorted(d.name for d in EXAMPLES.iterdir()
                      if d.is_dir() and (d / "README.md").exists())
    print(f"example portability: {len(examples)} examples, "
          f"target resolved in {RESOLVER}")
    if problems:
        print("\nExamples that would not survive FABRIC_TARGET=real:")
        for p in problems:
            print(f"  {p}")
        print(f"\nFAIL: {len(problems)} violation(s). docs/46-artifact-persistence.md "
              f"and docs/21-real-fabric-toggle.md are the contracts.")
        return 1
    print("every example resolves its target through the contract, and every "
          "definition part uses a Fabric source-format path")
    return 0


if __name__ == "__main__":
    sys.exit(main())
