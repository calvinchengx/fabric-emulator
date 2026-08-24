#!/usr/bin/env python3
"""Re-fetch and re-pin Microsoft's own `notebookutils` stubs.

    scripts/vendor_notebookutils_stubs.py [--version 1.6.3]

WHY VENDOR THIS. `docs/56`'s Axis A asks what `notebookutils.*` exposes and
whether each member carries the documented signature. Until now the answer came
from `e2e/conformance/notebookutils-reference.json`, transcribed by hand from
Learn pages, one module of nine. Transcription is the weakest link in an
evidence chain: it is a claim about Microsoft's surface with a person in the
middle, and it does not notice when the surface moves.

Microsoft publishes the surface itself. `dummy-notebookutils` is their stub
package — every function, every parameter name and default, empty bodies —
shipped so notebook code can be developed off-cluster. MIT, 12 KB. Pinning it
turns Axis A from a reading exercise into a diff.

WHAT IT IS NOT. It is Synapse-lineage (its own homepage is
`Azure/azure-synapse-analytics`, its summary says "synapse mssparkutils"), so it
is BROADER than Fabric: `conf`, `connections`, `data` and `fabricClient` are not
in Fabric's documented module list, and Fabric's own page says `fabricClient`
and `PBIClient` "aren't supported yet". The stub is the oracle for SIGNATURES;
Fabric's docs remain the oracle for SCOPE. `check_notebookutils_surface.py`
holds both, and refuses to guess when a new module appears in one and not the
other.

The refresh is a command rather than a ritual for the reason third_party/README
gives: a PROVENANCE table maintained by hand ships with stale hashes, and the
hash is the one field whose whole job is to be exact.
"""
import argparse
import hashlib
import io
import json
import pathlib
import re
import sys
import urllib.request
import zipfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
VENDOR = ROOT / "third_party" / "notebookutils-stubs"
PACKAGE = "dummy-notebookutils"
PYPI = f"https://pypi.org/pypi/{PACKAGE}/json"

# The rows this script owns in PROVENANCE.md, between the markers. Everything
# outside them is prose a person wrote and this must not touch.
BEGIN = "<!-- integrity:begin -->"
END = "<!-- integrity:end -->"


def wheel_url(version: str) -> tuple[str, str]:
    """The wheel for a version, and the upload date PyPI records for it."""
    with urllib.request.urlopen(PYPI, timeout=30) as response:
        index = json.load(response)
    files = index["releases"].get(version)
    if not files:
        raise SystemExit(
            f"vendor-notebookutils: {PACKAGE} has no release {version}. "
            f"Available: {', '.join(sorted(index['releases'])[-6:])}")
    for entry in files:
        if entry["filename"].endswith(".whl"):
            return entry["url"], entry["upload_time"][:10]
    raise SystemExit(f"vendor-notebookutils: {PACKAGE} {version} ships no wheel")


def extract(url: str) -> dict[str, bytes]:
    """The stub sources and the licence, keyed by the path they land at.

    Only `.py` under `notebookutils/` and the licence file: a wheel also
    carries RECORD and METADATA, which describe the distribution rather than
    the surface and would churn the integrity table on every rebuild.
    """
    with urllib.request.urlopen(url, timeout=60) as response:
        blob = response.read()
    out: dict[str, bytes] = {}
    with zipfile.ZipFile(io.BytesIO(blob)) as wheel:
        for name in wheel.namelist():
            if name.startswith("notebookutils/") and name.endswith(".py"):
                out[name] = wheel.read(name)
            elif name.endswith(".dist-info/licenses/LICENSE"):
                out["LICENSE"] = wheel.read(name)
    if "LICENSE" not in out:
        raise SystemExit(
            "vendor-notebookutils: the wheel carries no LICENSE. third_party/README "
            "requires the licence text beside what it covers; do not vendor without it.")
    if not any(n.endswith("notebook.py") for n in out):
        raise SystemExit(
            "vendor-notebookutils: no notebookutils/notebook.py in the wheel. The "
            "layout changed, and vendoring the rest would pin a surface with a hole "
            "in it.")
    return out


def write(files: dict[str, bytes]) -> None:
    for existing in sorted(VENDOR.rglob("*.py")):
        existing.unlink()
    for name, blob in files.items():
        target = VENDOR / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(blob)


def integrity_table(files: dict[str, bytes]) -> str:
    rows = ["| File | sha256 | Bytes |", "|---|---|---|"]
    for name in sorted(files):
        digest = hashlib.sha256(files[name]).hexdigest()
        rows.append(f"| `{name}` | `{digest}` | {len(files[name])} |")
    return "\n".join(rows)


def repin(version: str, url: str, uploaded: str, files: dict[str, bytes], today: str) -> None:
    path = VENDOR / "PROVENANCE.md"
    text = path.read_text(encoding="utf-8")
    for field, value in (
        ("Pinned revision", f"`{version}` (uploaded {uploaded})"),
        ("Retrieved", today),
        ("Wheel", f"{url}"),
    ):
        text = re.sub(rf"(\| \*\*{field}\*\* \| )[^|]*(\|)", rf"\g<1>{value} \g<2>", text)
    if BEGIN not in text or END not in text:
        raise SystemExit(f"vendor-notebookutils: {path} lost its integrity markers")
    head, rest = text.split(BEGIN, 1)
    _, tail = rest.split(END, 1)
    path.write_text(head + BEGIN + "\n" + integrity_table(files) + "\n" + END + tail,
                    encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--version", default="1.6.3")
    # Passed in rather than read from the clock, so a rerun on the same pin
    # produces the same bytes and the diff says "nothing moved".
    parser.add_argument("--today", default="", help="date to record as Retrieved")
    args = parser.parse_args()

    url, uploaded = wheel_url(args.version)
    files = extract(url)
    write(files)
    if args.today:
        repin(args.version, url, uploaded, files, args.today)
    else:
        print("no --today given: files refreshed, PROVENANCE.md left alone")
    print(f"vendor-notebookutils: {PACKAGE} {args.version} — "
          f"{len(files) - 1} stub file(s) + LICENSE")
    return 0


if __name__ == "__main__":
    sys.exit(main())
