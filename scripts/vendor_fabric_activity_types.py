#!/usr/bin/env python3
"""Re-fetch and re-pin Fabric's own DataPipelineActivityTypes table.

    scripts/vendor_fabric_activity_types.py [--check]

WHY THIS EXISTS. `internal/api/fabricActivityTypes` is the oracle the dispatch
is held to, and it was TRANSCRIBED BY HAND from Microsoft's article. The
structural test walks that list, so a type Fabric adds or renames is caught only
if a human notices the article changed and edits the list — which is the same
declared-list shape that produced the twelve fabricated successes, moved one
level up. Vendoring the table turns "someone re-read the docs" into a file with
a hash, and `check_fabric_activity_types.py` turns a drift into a failing test.

WHY THE PIN IS A CONTENT HASH AND NOT AN UPSTREAM COMMIT SHA. third_party's
pattern asks for a commit SHA, "never a moving branch/tag". That is not
available here: this article is a REST-API reference published only as generated
HTML — MicrosoftDocs/fabric-docs carries the Data Factory prose, but not this
page (checked: the docs-api path 404s). So the integrity pin is the sha256 of
the EXTRACTED TABLE plus the retrieval date, and the refresh command re-extracts
and diffs. That is weaker than a SHA in one specific way, stated rather than
glossed: it cannot tell an upstream edit from a fetch that went wrong, so a
diff is read, not merely accepted.
"""
import argparse
import datetime
import hashlib
import html as htmllib
import json
import pathlib
import re
import sys
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
VENDOR = ROOT / "third_party" / "fabric-activity-types"
ARTIFACT = VENDOR / "activity-types.json"
PROVENANCE = VENDOR / "PROVENANCE.md"
URL = ("https://learn.microsoft.com/en-us/rest/api/fabric/articles/"
       "item-management/definitions/datapipeline-definition")
SECTION = "DataPipelineActivityTypes"

# The table is <h3>DataPipelineActivityTypes</h3> followed by a two-column
# <table>. Anchored on the heading rather than on "the first table on the page":
# the page carries several, and position is not a contract.
ROW = re.compile(r"<tr>\s*<td>(.*?)</td>\s*<td>(.*?)</td>\s*</tr>", re.S)
TAG = re.compile(r"<[^>]+>")


def extract(page: str) -> dict[str, str]:
    i = page.find(f">{SECTION}</h")
    if i < 0:
        raise SystemExit(f"{SECTION} heading not found — the article's shape changed")
    start = page.find("<table>", i)
    end = page.find("</table>", start)
    if start < 0 or end < 0:
        raise SystemExit(f"no table after the {SECTION} heading")
    rows = {}
    for name, desc in ROW.findall(page[start:end]):
        n = htmllib.unescape(TAG.sub("", name)).strip()
        d = " ".join(htmllib.unescape(TAG.sub("", desc)).split())
        if n:
            rows[n] = d
    if len(rows) < 20:
        # A parse that silently returns three rows would vendor a table that
        # then "agrees" with nothing and quietly narrows the checker.
        raise SystemExit(f"extracted only {len(rows)} rows — refusing to vendor a partial table")
    return dict(sorted(rows.items()))


def render(rows: dict[str, str]) -> bytes:
    return (json.dumps({"source": URL, "section": SECTION, "types": rows},
                       indent=2, ensure_ascii=False) + "\n").encode()


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--check", action="store_true",
                    help="fetch and diff against the vendored copy; do not write")
    args = ap.parse_args()

    with urllib.request.urlopen(URL, timeout=60) as r:
        page = r.read().decode("utf-8", "replace")
    fresh = render(extract(page))
    digest = hashlib.sha256(fresh).hexdigest()

    if args.check:
        have = ARTIFACT.read_bytes() if ARTIFACT.exists() else b""
        if have == fresh:
            print(f"fabric activity types: vendored copy matches upstream ({digest[:12]}…)")
            return 0
        print("UPSTREAM HAS CHANGED — the vendored table no longer matches the article.")
        old = json.loads(have)["types"] if have else {}
        new = json.loads(fresh)["types"]
        for k in sorted(set(new) - set(old)):
            print(f"  + {k}")
        for k in sorted(set(old) - set(new)):
            print(f"  - {k}")
        for k in sorted(set(old) & set(new)):
            if old[k] != new[k]:
                print(f"  ~ {k}: {old[k]!r} -> {new[k]!r}")
        print("\nRe-pin with: scripts/vendor_fabric_activity_types.py")
        return 1

    VENDOR.mkdir(parents=True, exist_ok=True)
    ARTIFACT.write_bytes(fresh)
    today = datetime.datetime.now(datetime.UTC).date().isoformat()
    PROVENANCE.write_text(f"""# Fabric `DataPipelineActivityTypes` — pinned by content hash

Microsoft's own list of data-pipeline activity type discriminators: the oracle
`internal/api/fabricActivityTypes` is held to, and the reason the dispatch can
be checked for completeness at all.

**Why not a commit SHA.** `third_party/README.md` asks for a pinned upstream
revision. This artifact has none available: the article is a REST-API reference
published as generated HTML, and MicrosoftDocs/fabric-docs does not carry it
(the `docs-api/...` path 404s). The pin is therefore the sha256 of the extracted
table. That cannot distinguish an upstream edit from a bad fetch, so a `--check`
diff is **read**, never rubber-stamped.

## Provenance

- **Upstream:** {URL}
- **Section:** `{SECTION}`
- **Retrieved / pinned:** {today}
- **Integrity:** `sha256:{digest}`, {len(fresh)} bytes (`activity-types.json`)
- **License:** © Microsoft Corporation. Microsoft Learn content is published
  under CC-BY-4.0; only the type NAMES and their one-line descriptions are
  extracted, as the factual list this repo conforms to.
- **Used by:** `scripts/check_fabric_activity_types.py` (in `make check`), which
  holds `internal/api/fabricactivitytypes.go` to this table; that list in turn
  drives `TestEveryDocumentedFabricActivityTypeIsHandled`.

## Refresh

    scripts/vendor_fabric_activity_types.py            # re-fetch and re-pin
    scripts/vendor_fabric_activity_types.py --check    # diff without writing

`--check` needs the network and is deliberately NOT part of `make check`: the
offline checker compares the Go list against this file, so the gate stays
runnable with no network and an upstream change is a deliberate, reviewable
re-pin rather than a surprise red.
""")
    print(f"vendored {len(json.loads(fresh)['types'])} activity types, sha256:{digest[:12]}…")
    return 0


if __name__ == "__main__":
    sys.exit(main())
