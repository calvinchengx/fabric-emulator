#!/usr/bin/env python3
"""Every reference to a compute sidecar image must pin @sha256.

WHY THIS EXISTS. These images are tagged with the version of the dependency a
consumer pins them FOR -- emulator-sail:0.7.0 is the Sail engine,
emulator-spark-agent:4.2.0 is the Spark Connect client. That makes the tag
readable, and it makes it NOT an identity: both images also carry first-party
code (launcher.py, the whole spark_agent package) that changes independently of
those pins, so successive releases republish the same tag over different
content.

Under the old scheme the tag was the emulator's release version, so a bare tag
was effectively immutable and a missing digest cost nothing. That is no longer
true. A reference like `emulator-sail:0.7.0` with no digest now silently
follows whatever was published last -- the compose still parses, the stack
still starts, and the thing under test changed. That is the failure this
guards: not a broken pin, an invisibly floating one.

The rule is deliberately narrow, in two directions.

BY FILE: only files that can pull -- compose and env. Documentation naming the
image in a sentence pulls nothing, and demanding a digest there produces prose
no one can read and that goes stale every release. The first cut of this
checker scanned .md and flagged a README, which is how that was learned.

BY LINE: a tag that floats on purpose (`:dev`, `:latest`, an image built
locally) has no digest to pin, and pinning one would defeat it. Such a line
carries `digest-exempt: <reason>` and is skipped. The reason is required, and
it sits on the line it excuses rather than in a list that drifts away from it.

Third-party images keep their own conventions, and a `build:` service has no
tag to pin.
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
# The names are derived, not typed twice: image_tags.py owns the suffix list.
sys.path.insert(0, str(ROOT / "scripts"))
from image_tags import TAGGED_BY  # noqa: E402

IMAGES = tuple(f"emulator-{suffix}" for suffix in TAGGED_BY)
SKIP_DIRS = {".git", "node_modules", ".venv", "out", "dist", "__pycache__"}
# Only files that can actually PULL. Prose naming the image in a sentence
# cannot float a stack, and requiring a 64-hex digest inside documentation
# makes it unreadable and stale on every release -- the first version of this
# checker did exactly that, and flagged a README.
PULLING_SUFFIXES = {".yml", ".yaml", ".env"}
# A tag that is floating ON PURPOSE -- `:dev`, `:latest`, a locally built
# image -- has no digest to pin and pinning one would defeat it. Those lines
# opt out in place, with a reason, so the exemption is reviewable where it
# applies rather than in a list somewhere else.
EXEMPT = re.compile(r"digest-exempt:\s*(?P<reason>\S.*?)\s*$")
# <registry>/<org>/emulator-<suffix>:<tag> with an optional @sha256:<hex>.
#
# The lookbehind is load-bearing: without it `fabric-emulator-spark-agent`
# matches as a suffix of `emulator-spark-agent`, and the checker reports the
# OLD image name -- which is exactly the reference this rule does not govern.
REF = re.compile(
    r"(?<![A-Za-z0-9._-])(?P<name>" + "|".join(re.escape(i) for i in IMAGES) + r")"
    r":(?P<tag>[A-Za-z0-9._-]+)(?P<digest>@sha256:[0-9a-f]{64})?"
)


def offenders(root=ROOT):
    """Yield (file, line number, the unpinned reference)."""
    for path in sorted(root.rglob("*")):
        if not path.is_file() or any(p in SKIP_DIRS for p in path.parts):
            continue
        if path.suffix not in PULLING_SUFFIXES:
            continue
        try:
            text = path.read_text(encoding="utf-8")
        except (UnicodeDecodeError, OSError):
            continue
        for n, line in enumerate(text.splitlines(), 1):
            exemption = EXEMPT.search(line)
            for m in REF.finditer(line):
                if m.group("digest") is not None or exemption:
                    continue
                yield path.relative_to(root), n, m.group(0)


def main():
    bad = list(offenders())
    if not bad:
        print(f"every {', '.join(IMAGES)} reference pins @sha256")
        return 0
    print("references to a compute sidecar image without @sha256:\n")
    for path, n, ref in bad:
        print(f"  {path}:{n}  {ref}")
    print(
        "\nThe tag is the dependency version, not an identity: the same tag is"
        "\nrepublished over new first-party code. Add @sha256:<digest> so the"
        "\nreference names one build. `docker buildx imagetools inspect <ref>`"
        "\nprints it."
    )
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
