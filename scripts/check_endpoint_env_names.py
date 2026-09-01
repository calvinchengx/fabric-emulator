#!/usr/bin/env python3
"""An endpoint override must name a variable fabric-target actually reads.

WHY THIS EXISTS. Endpoint resolution moved into `python/fabric-target`, which
reads FABRIC_EMULATOR_URL/FABRIC_URL, ENTRA_EMULATOR_URL/ENTRA_URL and
VAULT_EMULATOR_URL/AZURE_KEY_VAULT_URL. Three callers kept setting the names
from before the move -- `FABRIC_REST_URL` and `KV_URL` -- which nothing on that
path reads. Both silently fell back to their defaults.

THE DEFAULTS ARE WHY NOBODY NOTICED: they are exactly the ports the CI stack
publishes, so an override that did nothing looked like an override that worked.
It only mattered for a stack that REMAPPED its ports, and that is the case CI
never runs.

Where it did matter, it mattered badly. `docs/demo/flow-override.yml` remaps
every published port because a sibling project holding 9443 collided with the
recording -- and the remap was wired with `FABRIC_REST_URL`, so the recording
asked for 9843 and then talked to whatever answered on 9443. The one variable
that had to work was the one being ignored.

WHAT THIS CHECKS. Every *_URL-ish endpoint name set by a harness or recording
must appear in fabric-target's accepted set. The accepted set is READ FROM THE
SOURCE rather than restated here, so adding an alias there cannot make this
check stale.

Usage:
    check_endpoint_env_names.py      exit non-zero naming any unread override
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
TARGET = ROOT / "python" / "fabric-target" / "fabric_target" / "__init__.py"

# Files that point an example at a stack. Each sets endpoint variables that the
# example resolves through fabric-target.
CALLERS = [
    pathlib.Path("e2e") / "medallion" / "run.py",
    pathlib.Path("docs") / "demo" / "flow.py",
]

# Names these files legitimately set that fabric-target does NOT own: they are
# read directly by examples/contoso-fixtures/common.py instead.
COMMON_OWNED = {"TDS_SERVER", "KV_INTERNAL_URL", "SPARK_REMOTE", "OM_URL",
                "PIPELINE_STATE", "GOLD_PROJECT", "WORKSPACE_NAME",
                "DEFINITIONS"}

# Only endpoint-shaped names are in scope; a caller may set anything else.
ENDPOINTISH = re.compile(r"^[A-Z0-9_]*(URL|SERVER|REMOTE)$")


def accepted():
    """The endpoint names fabric-target reads, from its own source."""
    text = TARGET.read_text(encoding="utf-8")
    names = set()
    for call in re.findall(r"_env_any\(\(([^)]*)\)", text, re.S):
        names.update(re.findall(r'"([A-Z0-9_]+)"', call))
    names.update(re.findall(r'_env\(\s*"([A-Z0-9_]+)"', text))
    return names


def set_by(path):
    """Endpoint-shaped variables a caller puts into a child's environment."""
    text = (ROOT / path).read_text(encoding="utf-8")
    return {n for n in re.findall(r'"([A-Z0-9_]+)"\s*:', text) if ENDPOINTISH.match(n)}


def main() -> int:
    ok = accepted() | COMMON_OWNED
    bad = []
    for path in CALLERS:
        for name in sorted(set_by(path)):
            if name not in ok:
                bad.append(f"{path}: {name}")
    if bad:
        print("check_endpoint_env_names: these overrides name a variable "
              "nothing reads, so they silently fall back to the default:\n  "
              + "\n  ".join(bad)
              + "\n\nfabric-target accepts: " + ", ".join(sorted(accepted()))
              + "\nA stack on the default ports hides this; a remapped one "
                "does not.", file=sys.stderr)
        return 1
    print(f"check_endpoint_env_names: {len(CALLERS)} callers, every endpoint "
          f"override names a variable fabric-target reads")
    return 0


if __name__ == "__main__":
    sys.exit(main())
