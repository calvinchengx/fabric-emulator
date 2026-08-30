#!/usr/bin/env python3
"""Re-fetch and re-pin ADF/Synapse's `entityTypes/Pipeline.json`.

    scripts/vendor_adf_pipeline_schema.py [--sha <commit>]

WHY. The emulator accepts ADF's activity vocabulary as a COMPATIBILITY surface
(`RunNotebook`, `AzureFunctionActivity`, `DatabricksSparkJar`, the HDInsight
family, …) alongside Fabric's own. Fabric's list is vendored and checked
(third_party/fabric-activity-types). ADF's was not: it lived in hand-written
lists in Go, which is the shape that let twelve Fabric types reach the success
stub before anyone noticed.

Unlike the Fabric article, this one CAN be pinned properly — it is a file in a
public git repo, so the pin is a commit SHA as third_party/README.md asks,
not a content hash.
"""
import argparse
import hashlib
import pathlib
import sys
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
VENDOR = ROOT / "third_party" / "adf-pipeline-schema"
REPO = "Azure/azure-rest-api-specs"
PATH = ("specification/synapse/data-plane/Microsoft.Synapse/preview/"
        "2021-06-01-preview/entityTypes/Pipeline.json")
DEFAULT_SHA = "e773d51070bcccd61bafc30ccfedb96b261a0754"


def fetch(sha: str, path: str) -> bytes:
    url = f"https://raw.githubusercontent.com/{REPO}/{sha}/{path}"
    with urllib.request.urlopen(url, timeout=60) as r:
        return r.read()


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--sha", default=DEFAULT_SHA, help="upstream commit to pin")
    args = ap.parse_args()

    schema = fetch(args.sha, PATH)
    licence = fetch(args.sha, "LICENSE")
    if len(schema) < 100_000:
        raise SystemExit(f"schema is only {len(schema)} bytes — refusing to vendor a truncated fetch")

    VENDOR.mkdir(parents=True, exist_ok=True)
    (VENDOR / "Pipeline.json").write_bytes(schema)
    (VENDOR / "LICENSE").write_bytes(licence)
    digest = hashlib.sha256(schema).hexdigest()
    (VENDOR / "PROVENANCE.md").write_text(f"""# ADF/Synapse `entityTypes/Pipeline.json` — copied in full

The published schema for Data Factory / Synapse pipeline **activities**: the
`x-ms-discriminator-value` of every activity type ADF defines. The emulator
accepts this vocabulary as a compatibility surface beside Fabric's own
(third_party/fabric-activity-types), so it is an oracle in exactly the same way
and is now checked in exactly the same way.

## Provenance

- **Upstream:** https://github.com/{REPO}
- **File:** `{PATH}`
- **Pinned revision:** `{args.sha}`
- **Integrity:** `sha256:{digest}`, {len(schema)} bytes (`Pipeline.json`)
- **License:** MIT (`LICENSE`) — © Microsoft Corporation.
- **Used by:** `scripts/check_adf_activity_types.py` (in `make check` and CI),
  which asserts every CONCRETE ADF activity type is handled by the dispatch,
  the pipeline interpreter, or the refusal map — never the success stub.

## Concrete vs abstract, and why the checker derives it

Three of the 41 discriminators are not authorable activity types: `Container`
(`ControlActivity`) and `Execution` (`ExecutionActivity`) are **base classes**
other definitions inherit from. The checker excludes them by DERIVING which
definitions are `allOf` bases, rather than carrying an exemption list — an
exemption list is the thing this whole exercise exists to remove.

## Refresh

    scripts/vendor_adf_pipeline_schema.py --sha <newer-commit>

Bump deliberately, in its own commit, and diff the schema: a change here is a
change to the golden truth the dispatch is measured against.
""")
    print(f"vendored Pipeline.json at {args.sha[:12]}, sha256:{digest[:12]}…, {len(schema)} bytes")
    return 0


if __name__ == "__main__":
    sys.exit(main())
