#!/usr/bin/env python3
"""Every CONCRETE ADF activity type must be handled — never the success stub.

The emulator accepts ADF's activity vocabulary beside Fabric's own, so an ADF
type it does not recognise falls to the dispatch default and is reported
`{"status": "Succeeded"}` having done nothing. That is the defect that hit
twelve Fabric types; the ADF half was still guarded only by hand-written lists
in Go, and a hand-written list is what nothing checks.

"Handled" means one of three real outcomes, and the checker does not care which:

  * a `case` in `internal/api/pipelines.go`'s dispatch — it runs, or fails on
    its own properties;
  * a `case` in `internal/pipeline/activities.go` — control flow, interpreted
    before the executor sees it;
  * a key in `unrunnableActivities` — refused by name, with a cause.

ABSTRACT BASES ARE EXCLUDED BY DERIVATION, NOT BY EXEMPTION. `Container`
(`ControlActivity`) and `Execution` (`ExecutionActivity`) carry discriminators
but are base classes other definitions inherit from; they are not authorable.
The checker finds them by walking `allOf` and asking which definitions are
someone else's base — because an exemption list here would be the very thing
this file exists to delete.
"""
import hashlib
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
VENDOR = ROOT / "third_party" / "adf-pipeline-schema"
SCHEMA = VENDOR / "Pipeline.json"
PROVENANCE = VENDOR / "PROVENANCE.md"
DISPATCH = ROOT / "internal" / "api" / "pipelines.go"
INTERP = ROOT / "internal" / "pipeline" / "activities.go"
REFUSALS = ROOT / "internal" / "api" / "unrunnableactivities.go"

SHA = re.compile(r"`sha256:([0-9a-f]{64})`")
CASE = re.compile(r'^\tcase ((?:"[A-Za-z0-9-]+"(?:, )?)+):', re.M)
QUOTED = re.compile(r'"([A-Za-z0-9-]+)"')
REFUSAL_KEY = re.compile(r'^\t"([A-Za-z0-9-]+)":', re.M)


def concrete_types(schema: dict) -> dict[str, str]:
    """Discriminator -> definition name, for definitions nothing inherits from."""
    defs = schema["definitions"]
    bases = set()
    for v in defs.values():
        for a in (v.get("allOf") or []) if isinstance(v, dict) else []:
            ref = a.get("$ref", "")
            if ref.startswith("#/definitions/"):
                bases.add(ref.rsplit("/", 1)[-1])
    out = {}
    for name, v in defs.items():
        if not isinstance(v, dict):
            continue
        disc = v.get("x-ms-discriminator-value")
        if disc and name not in bases:
            out[disc] = name
    return out


def handled(text_dispatch: str, text_interp: str, text_refusals: str) -> set[str]:
    names: set[str] = set()
    for m in CASE.finditer(text_dispatch):
        names |= set(QUOTED.findall(m.group(1)))
    names |= set(re.findall(r'case "([A-Za-z0-9-]+)"', text_interp))
    names |= set(REFUSAL_KEY.findall(text_refusals))
    return names


def main() -> int:
    for p in (SCHEMA, PROVENANCE, DISPATCH, INTERP, REFUSALS):
        if not p.exists():
            print(f"check_adf_activity_types: missing {p.relative_to(ROOT)}")
            return 1

    raw = SCHEMA.read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    pinned = SHA.search(PROVENANCE.read_text(encoding="utf-8"))
    if not pinned:
        print("check_adf_activity_types: PROVENANCE.md records no sha256")
        return 1
    if pinned.group(1) != digest:
        print("check_adf_activity_types: the vendored schema does not match its own hash.\n"
              f"  PROVENANCE.md: sha256:{pinned.group(1)}\n"
              f"  Pipeline.json: sha256:{digest}\n"
              "Re-pin deliberately with scripts/vendor_adf_pipeline_schema.py and read the diff.")
        return 1

    types = concrete_types(json.loads(raw))
    if len(types) < 30:
        print(f"check_adf_activity_types: only {len(types)} concrete types found — "
              "the schema's shape changed and this checker is reading it wrong, which "
              "would silently narrow the guard")
        return 1

    known = handled(DISPATCH.read_text(encoding="utf-8"),
                    INTERP.read_text(encoding="utf-8"),
                    REFUSALS.read_text(encoding="utf-8"))
    gap = sorted(t for t in types if t not in known)
    if gap:
        print("check_adf_activity_types: these ADF activity types are handled nowhere, so a "
              "pipeline using one is told it Succeeded having run nothing:\n")
        for t in gap:
            print(f"  {t}   ({types[t]})")
        print("\nEach needs a dispatch case, an interpreter case, or an entry in "
              "unrunnableActivities with its cause.")
        return 1

    print(f"check_adf_activity_types: {len(types)} concrete ADF types, all handled "
          f"(dispatch, interpreter, or refused by name)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
