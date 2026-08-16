#!/usr/bin/env python3
"""The dependency version each compute image is tagged with.

WHY THIS EXISTS. The sidecar images are named for what a consumer pins them
FOR, not for the repo that happens to publish them: emulator-sail carries the
Sail engine, emulator-spark-agent carries the Spark Connect client. So the tag
has to be the version of that dependency — and that version already has exactly
one home, the pins in pyproject.toml.

Reading it here rather than writing it into the workflow is the point. A tag
typed into release.yml is a second copy of a number, and the release that bumps
pysail without also editing the workflow publishes an image whose tag names the
version it no longer contains. Nothing fails; the tag simply lies.

WHAT THE TAG DOES NOT SAY. Both images also carry first-party code — sail has
launcher.py, spark-agent has the whole spark_agent package — which changes
independently of these pins. So a tag is NOT an identity: two releases can
publish emulator-sail:0.7.0 with different launcher code. That is deliberate,
and it is why every reference to these images must pin @sha256 as well
(scripts/check_image_digests.py enforces it) and why the release version is
carried in the OCI labels the metadata action already emits.
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
PYPROJECT = ROOT / "pyproject.toml"

# image name suffix -> the pin whose version it is tagged with
TAGGED_BY = {
    "sail": "pysail",
    "spark-agent": "pyspark-client",
}


def version_of(package, text=None):
    """The pinned version of `package`, read from pyproject.toml."""
    text = PYPROJECT.read_text(encoding="utf-8") if text is None else text
    found = re.findall(rf'"{re.escape(package)}==([0-9][^"]*)"', text)
    if not found:
        raise SystemExit(f"{package} is not pinned with == in pyproject.toml")
    if len(set(found)) > 1:
        raise SystemExit(f"{package} is pinned to several versions: {sorted(set(found))}")
    return found[0]


def main(argv):
    if len(argv) == 2 and argv[1] in TAGGED_BY:
        print(version_of(TAGGED_BY[argv[1]]))
        return 0
    for image, package in TAGGED_BY.items():
        print(f"{image}={version_of(package)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
