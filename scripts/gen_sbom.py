#!/usr/bin/env python3
"""Produce a CycloneDX SBOM of every locked dependency in this repository.

WHY THIS EXISTS. SECURITY.md recorded, twice, that nothing produced an
inventory a downstream consumer could ingest. The scanners in `security.yml`
and `check_dependency_risk.py` answer "is anything we depend on known-bad";
none of them answers "what do we depend on", which is the question a consumer
pulling the published image asks first.

WHAT IT READS. Only resolved, committed lock data -- never the network, never
an installed environment, so the output is the same on every machine:

  * `go.mod`        every `require` (direct and `// indirect`)  -> pkg:golang
  * `uv.lock`       every tracked one, registry/git/url sources  -> pkg:pypi
  * `pnpm-lock.yaml` the `packages:` table                       -> pkg:npm

It is stdlib-only, like every sibling under scripts/. pnpm's lock is YAML and
there is no YAML parser here, so the `packages:` keys are read by shape; the
test asserts that shape against this repository's own lockfile so a pnpm
format change fails loudly instead of yielding an empty inventory.

WHAT IT DOES NOT DO. It does not resolve licences (the lockfiles do not carry
them), does not read Dockerfiles or GitHub Actions pins (neither is a resolved
dependency set -- the same reason check_dependency_risk.py gives), and does
not model the dependency graph, only membership.

DETERMINISM. No timestamp, and the serial number is a uuid5 of the component
list, so regenerating from an unchanged tree is byte-identical and a diff of
two SBOMs is a diff of the dependencies.

Exit status is 0, or 1 when a manifest is present but yielded no components --
a parser that quietly reads nothing is the failure this guards against.
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
import tomllib
import urllib.parse
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
_NS = uuid.UUID("6b1f1c2e-5d0a-4a57-9f53-0d6d8f3a9c11")

_GO_REQ = re.compile(r"^\s*([^\s()]+)\s+(v[^\s]+)(\s*//\s*indirect)?\s*$")
_PNPM_KEY = re.compile(r"^  '?(@?[^@'\s]+)@([^':\s]+)'?:\s*$")
_PYPI_NORM = re.compile(r"[-_.]+")


def _purl(kind: str, name: str, version: str) -> str:
    if kind == "npm" and name.startswith("@"):
        scope, _, rest = name.partition("/")
        name = f"%40{scope[1:]}/{urllib.parse.quote(rest)}"
    elif kind == "pypi":
        name = _PYPI_NORM.sub("-", name).lower()
    else:
        name = urllib.parse.quote(name, safe="/")
    return f"pkg:{kind}/{name}@{urllib.parse.quote(version, safe='.-+')}"


def go_components(text: str) -> list[dict]:
    """`require` lines from a go.mod, in block or single-line form."""
    out, in_block = [], False
    for line in text.splitlines():
        s = line.strip()
        if s.startswith("require ("):
            in_block = True
            continue
        if in_block and s == ")":
            in_block = False
            continue
        if in_block:
            m = _GO_REQ.match(line)
        elif s.startswith("require "):
            m = _GO_REQ.match(s[len("require "):])
        else:
            continue
        if m:
            out.append({"kind": "golang", "name": m[1], "version": m[2],
                        "indirect": bool(m[3])})
    return out


def uv_components(text: str) -> list[dict]:
    """Registry/git/url packages from a uv.lock; the project's own members
    (virtual/editable/directory sources) are this repository, not a dependency."""
    out = []
    for pkg in tomllib.loads(text).get("package", []):
        source = pkg.get("source", {})
        if not ({"registry", "git", "url"} & source.keys()) or "version" not in pkg:
            continue
        out.append({"kind": "pypi", "name": pkg["name"],
                    "version": pkg["version"], "indirect": False})
    return out


def pnpm_components(text: str) -> list[dict]:
    """The `packages:` table of a pnpm v9 lockfile."""
    out, in_packages = [], False
    for line in text.splitlines():
        if line.startswith("packages:"):
            in_packages = True
            continue
        if in_packages and line and not line.startswith(" "):
            break
        m = _PNPM_KEY.match(line) if in_packages else None
        if m:
            out.append({"kind": "npm", "name": m[1], "version": m[2],
                        "indirect": False})
    return out


_PARSERS = {
    "go.mod": go_components,
    "uv.lock": uv_components,
    "pnpm-lock.yaml": pnpm_components,
}


def tracked_manifests(root: Path) -> list[str]:
    out = subprocess.run(["git", "ls-files"], cwd=root, check=True,
                         capture_output=True, text=True).stdout.split()
    return sorted(p for p in out if Path(p).name in _PARSERS)


def build(root: Path, manifests: list[str]) -> tuple[dict, list[str]]:
    """Return (bom, empty_manifests)."""
    seen: dict[str, dict] = {}
    empty = []
    for rel in manifests:
        found = _PARSERS[Path(rel).name]((root / rel).read_text(encoding="utf-8"))
        if not found:
            empty.append(rel)
        for c in found:
            purl = _purl(c["kind"], c["name"], c["version"])
            comp = seen.setdefault(purl, {
                "type": "library", "bom-ref": purl, "name": c["name"],
                "version": c["version"].removeprefix("v") if c["kind"] == "golang"
                else c["version"],
                "purl": purl, "_m": set(), "_i": True})
            comp["_m"].add(rel)
            comp["_i"] = comp["_i"] and c["indirect"]
    components: list[dict] = []
    for purl in sorted(seen):
        c = seen[purl]
        props = [{"name": "fabric:manifest", "value": m} for m in sorted(c.pop("_m"))]
        if c.pop("_i"):
            props.append({"name": "fabric:scope", "value": "indirect"})
        components.append({**c, "properties": props})
    serial = uuid.uuid5(_NS, "\n".join(str(c["purl"]) for c in components))
    return ({"bomFormat": "CycloneDX", "specVersion": "1.5",
             "serialNumber": f"urn:uuid:{serial}", "version": 1,
             "metadata": {"component": {"type": "application",
                                        "name": "fabric-emulator"}},
             "components": components}, empty)


def main(argv: list[str] | None = None, root: Path = ROOT) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--out", help="write here instead of stdout")
    args = ap.parse_args(argv)
    bom, empty = build(root, tracked_manifests(root))
    text = json.dumps(bom, indent=2, sort_keys=True) + "\n"
    if args.out:
        Path(args.out).write_text(text, encoding="utf-8")
    else:
        sys.stdout.write(text)
    print(f"sbom: {len(bom['components'])} components", file=sys.stderr)
    for rel in empty:
        print(f"sbom: {rel} yielded no components -- parser drift?", file=sys.stderr)
    return 1 if empty else 0


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
