#!/usr/bin/env python3
"""Fetch the classpath for Apache Spark's JVM Spark Connect client.

WHY THIS EXISTS. Sail is a Spark Connect *server*, so a JVM that wants to drive
it needs a Connect *client* and no cluster at all. That client is one Maven
coordinate plus its dependencies, and `e2e/sail-jvm-client` needs them on disk
before it can compile or run anything.

DERIVED, NOT DECLARED. Spark publishes FLATTENED poms: every transitive is
already listed in the client's own pom, versions and all. So the classpath is
read from that pom at fetch time rather than copied into a list here. A
hand-written list would be one more thing nothing checks, and this repo has been
bitten by exactly that (`docs/policy-inventory`, the activity-type tables).

WHY 4.1.2 AND NOT 4.2.0. Sail 0.7.1 is pinned against pyspark-client 4.2.0, but
Maven Central publishes `spark-connect-client-jvm_2.13` 4.2.0 only as previews;
4.1.2 is the newest stable. Reaching back one minor is not a guess: pyproject's
`sail` group records a measurement that Sail 0.7.0 also round-trips a 4.1.1
client, so the server tolerates a client one minor behind. Measured again here
end to end by e2e/sail-jvm-client.
"""

import os
import sys
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET

CENTRAL = "https://repo1.maven.org/maven2"
NS = "{http://maven.apache.org/POM/4.0.0}"

# The one pinned coordinate. Everything else on the classpath is derived from
# its pom, so a bump is this line and nothing else.
GROUP, ARTIFACT, VERSION = "org.apache.spark", "spark-connect-client-jvm_2.13", "4.1.2"

# A floor, not a count. The pom resolved to 51 compile/runtime deps when this
# was written; the exact number may move with a bump and that is fine. What is
# NOT fine is a handful, which means the pom shape changed or a fetch was
# truncated — and a short classpath fails later as NoClassDefFoundError, which
# reads as a broken probe rather than a broken download.
MIN_DEPS = 40


def _get(url: str, attempts: int = 5) -> bytes:
    """GET with retries. Maven Central rate-limits GitHub runners by shared IP,
    which is why scripts/fetch_spark_jars.sh retries too."""
    last: Exception | None = None
    for _attempt in range(attempts):
        try:
            with urllib.request.urlopen(url, timeout=120) as r:
                return r.read()
        except urllib.error.HTTPError as e:
            if e.code == 404:
                raise
            last = e
        except Exception as e:  # noqa: BLE001 - retried below, re-raised if final
            last = e
    raise RuntimeError(f"giving up on {url} after {attempts} attempts: {last}")


def _path(group: str, artifact: str, version: str, ext: str) -> str:
    return f"{CENTRAL}/{group.replace('.', '/')}/{artifact}/{version}/{artifact}-{version}.{ext}"


def _pom(group: str, artifact: str, version: str) -> ET.Element:
    return ET.fromstring(_get(_path(group, artifact, version, "pom")))


def _dependencies(root: ET.Element) -> list[tuple[str, str, str, str]]:
    block = root.find(NS + "dependencies")
    out = []
    for d in block if block is not None else []:
        def field(tag: str, dep: ET.Element = d) -> str:
            return (dep.findtext(NS + tag) or "").strip()

        out.append((field("groupId"), field("artifactId"), field("version"),
                    field("scope") or "compile"))
    return out


def classpath_coordinates() -> list[tuple[str, str, str]]:
    """The client plus every compile/runtime dependency its pom names."""
    client = _pom(GROUP, ARTIFACT, VERSION)

    # Properties and dependencyManagement resolve the few deps whose version is
    # a ${property} or inherited rather than literal.
    properties = {"project.version": VERSION}
    managed: dict[tuple[str, str], str] = {}
    parent = client.find(NS + "parent")
    if parent is not None:
        pp = _pom(*(parent.findtext(NS + t).strip() for t in ("groupId", "artifactId", "version")))
        for holder in (client, pp):
            block = holder.find(NS + "properties")
            for e in block if block is not None else []:
                properties.setdefault(e.tag.replace(NS, ""), (e.text or "").strip())
        dm = pp.find(NS + "dependencyManagement")
        if dm is not None:
            managed = {(g, a): v for g, a, v, _ in _dependencies(dm)}

    def resolve(value: str) -> str:
        for _ in range(5):
            if not (value.startswith("${") and value.endswith("}")):
                break
            value = properties.get(value[2:-1], "")
        return value

    coords = [(GROUP, ARTIFACT, VERSION)]
    unresolved = []
    for group, artifact, version, scope in _dependencies(client):
        if scope not in ("compile", "runtime"):
            continue
        version = resolve(version) or resolve(managed.get((group, artifact), ""))
        (coords if version else unresolved).append((group, artifact, version))
    if unresolved:
        raise RuntimeError(f"no version for: {', '.join(a for _, a, _ in unresolved)}")
    if len(coords) < MIN_DEPS:
        raise RuntimeError(
            f"{ARTIFACT} {VERSION} resolved to only {len(coords)} jars (floor {MIN_DEPS}); "
            "the pom shape changed, do not proceed on a truncated classpath")
    return coords


def fetch(dest: str) -> list[str]:
    """Download anything missing. Idempotent, so a cache hit costs nothing."""
    os.makedirs(dest, exist_ok=True)
    files, fetched = [], 0
    for group, artifact, version in classpath_coordinates():
        name = f"{artifact}-{version}.jar"
        target = os.path.join(dest, name)
        if not (os.path.exists(target) and os.path.getsize(target) > 0):
            part = target + ".part"
            with open(part, "wb") as fh:
                fh.write(_get(_path(group, artifact, version, "jar")))
            os.replace(part, target)
            fetched += 1
        files.append(target)
    print(f"connect client jars: {fetched} fetched, {len(files) - fetched} cached", file=sys.stderr)
    return files


if __name__ == "__main__":
    where = sys.argv[1] if len(sys.argv) > 1 else "e2e/sail-jvm-client/jars"
    print(os.pathsep.join(fetch(where)))
