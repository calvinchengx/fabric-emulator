"""The pom parsing in fetch_connect_client_jars is load-bearing, so it is tested
rather than exempted from coverage.

A truncated or mis-parsed classpath does not fail here: it fails much later as a
NoClassDefFoundError inside a JVM, which reads as a broken probe rather than a
broken download. Same reasoning that took vendor_fabric_activity_types.py off
the coverage omit list — the extraction IS the part that can be wrong.
"""

import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(
    os.path.abspath(__file__)))), "scripts"))

import fetch_connect_client_jars as f  # noqa: E402

P = "http://maven.apache.org/POM/4.0.0"


def pom(deps: str, properties: str = "", parent: bool = True) -> bytes:
    par = ('<parent><groupId>org.apache.spark</groupId>'
           '<artifactId>spark-parent_2.13</artifactId><version>4.1.2</version></parent>'
           if parent else "")
    return (f'<project xmlns="{P}">{par}<properties>{properties}</properties>'
            f'<dependencies>{deps}</dependencies></project>').encode()


def dep(artifact: str, version: str = "1.0", scope: str = "compile") -> str:
    return (f"<dependency><groupId>g</groupId><artifactId>{artifact}</artifactId>"
            f"<version>{version}</version><scope>{scope}</scope></dependency>")


def wire(monkeypatch, client: bytes, parent: bytes = b"") -> None:
    """Serve the client pom, then the parent pom, from memory."""
    parent = parent or pom("", parent=False)

    def fake(url: str, attempts: int = 5) -> bytes:
        if "spark-parent" in url:
            return parent
        if url.endswith(".pom"):
            return client
        return b"JAR"

    monkeypatch.setattr(f, "_get", fake)


def many(n: int) -> str:
    return "".join(dep(f"a{i}") for i in range(n))


def test_test_and_provided_scopes_are_not_on_the_runtime_classpath(monkeypatch):
    wire(monkeypatch, pom(many(45) + dep("junit", scope="test")
                          + dep("hadoop", scope="provided")))
    names = [a for _, a, _ in f.classpath_coordinates()]
    assert "junit" not in names and "hadoop" not in names
    # The client itself is always first: it is the coordinate everything else
    # was derived from, and Probe.java is compiled against it.
    assert names[0] == f.ARTIFACT


def test_a_property_version_resolves_from_the_parent(monkeypatch):
    wire(monkeypatch,
         pom(many(44) + dep("icu4j", version="${icu.version}")),
         parent=pom("", properties="<icu.version>76.1</icu.version>", parent=False))
    assert ("g", "icu4j", "76.1") in f.classpath_coordinates()


def test_project_version_resolves_to_the_pinned_client_version(monkeypatch):
    wire(monkeypatch, pom(many(44) + dep("spark-sketch", version="${project.version}")))
    assert ("g", "spark-sketch", f.VERSION) in f.classpath_coordinates()


def test_a_version_that_resolves_to_nothing_is_fatal(monkeypatch):
    wire(monkeypatch, pom(many(44) + dep("mystery", version="${nowhere}")))
    with pytest.raises(RuntimeError, match="no version for: mystery"):
        f.classpath_coordinates()


def test_a_short_classpath_is_refused_rather_than_returned(monkeypatch):
    """The floor is the guard that turns a silent truncation into a failure."""
    wire(monkeypatch, pom(many(3)))
    with pytest.raises(RuntimeError, match="do not proceed on a truncated classpath"):
        f.classpath_coordinates()


def test_fetch_is_idempotent_and_leaves_no_part_files(monkeypatch, tmp_path):
    wire(monkeypatch, pom(many(45)))
    first = f.fetch(str(tmp_path))
    assert len(first) == 46 and all(os.path.getsize(p) for p in first)

    def explode(url: str, attempts: int = 5) -> bytes:
        if url.endswith(".pom"):
            return pom(many(45))
        raise AssertionError(f"re-downloaded {url} despite a populated cache")

    monkeypatch.setattr(f, "_get", explode)
    assert f.fetch(str(tmp_path)) == first
    assert not list(tmp_path.glob("*.part"))


def test_an_empty_cached_file_is_refetched(monkeypatch, tmp_path):
    """A zero-byte jar is what a killed download leaves behind, and it would
    otherwise be treated as cached forever."""
    wire(monkeypatch, pom(many(45)))
    (tmp_path / "a0-1.0.jar").write_bytes(b"")
    f.fetch(str(tmp_path))
    assert (tmp_path / "a0-1.0.jar").read_bytes() == b"JAR"
