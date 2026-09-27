"""Tests for the compute-sidecar tag resolver.

WHAT THIS FILE IS GUARDING, in the script's own words: "A tag typed into
release.yml is a second copy of a number, and the release that bumps pysail
without also editing the workflow publishes an image whose tag names the version
it no longer contains. Nothing fails; the tag simply lies." So the thing at risk
is not a crash -- it is a WRONG ANSWER that looks like a right one, which is the
only failure mode `version_of` has.

Every case below drives `version_of` over INLINE pyproject text rather than over
this repository's own pyproject.toml. That is not tidiness: a test reading the
real pins asserts today's numbers, so it fails on the next legitimate pysail bump
and teaches whoever is doing the bump to edit the test. A test that must be
edited to stay true is a test people stop reading.

THE TWO REFUSALS ARE THE POINT. `version_of` raises rather than returning
something plausible when a package is unpinned or pinned twice, and both of those
are the shape this repository keeps producing: an absence reported as a presence.
A resolver that answered "" for an unpinned package would publish
`emulator-sail:` and the metadata action would take the empty tag as no tag at
all.
"""
import pathlib
import re
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import image_tags as t  # noqa: E402

REPO = pathlib.Path(__file__).resolve().parents[2]

# The shape of a real pin, reduced to the two lines that matter. The dependency
# group wrapper is kept because the regex is deliberately not anchored to it --
# these pins live under several groups and the resolver must not care which.
PINNED = '''\
[project]
name = "fabric-emulator"

[dependency-groups]
spark-client = ["pyspark-client==4.2.0"]
sail-delta = ["pysail==0.7.0", "deltalake==1.2.1"]
'''


def test_a_pinned_version_is_read_from_the_text():
    assert t.version_of("pysail", PINNED) == "0.7.0"
    assert t.version_of("pyspark-client", PINNED) == "4.2.0"


def test_a_neighbouring_pin_is_not_read_by_mistake():
    """`pysail` must not be satisfied by `pyspark-client`, and a substring must
    not match: the names in this file share a prefix, and the tag being silently
    the OTHER image's version is indistinguishable from it being right."""
    assert t.version_of("pysail", PINNED) != t.version_of("pyspark-client", PINNED)
    with pytest.raises(SystemExit):
        t.version_of("pysai", PINNED)


def test_an_unpinned_package_refuses_rather_than_answering_empty():
    """The whole class of defect this repository keeps hitting: an absence
    reported as a presence. An empty string here publishes `emulator-sail:` and
    the metadata action reads that as no tag at all."""
    with pytest.raises(SystemExit) as e:
        t.version_of("pysail", '[dependency-groups]\nsail = ["pysail"]\n')
    assert "not pinned" in str(e.value)


def test_a_range_is_not_a_pin():
    """`>=` is not `==`. A range resolves to different versions over time, which
    is exactly the floating the tag is supposed to rule out."""
    with pytest.raises(SystemExit):
        t.version_of("pysail", '[dependency-groups]\nsail = ["pysail>=0.7.0"]\n')


def test_two_conflicting_pins_refuse_rather_than_picking_one():
    """Taking the first would make the tag a function of file order. Two groups
    disagreeing about a version is a real state this file can reach -- the lint
    and sail-delta groups both name pyspark-client's siblings -- and either
    answer would be wrong half the time."""
    text = ('[dependency-groups]\n'
            'a = ["pysail==0.7.0"]\n'
            'b = ["pysail==0.8.0"]\n')
    with pytest.raises(SystemExit) as e:
        t.version_of("pysail", text)
    assert "several versions" in str(e.value)


def test_the_same_version_pinned_twice_is_not_a_conflict():
    """A near-miss the refusal above must NOT catch: two groups agreeing is
    normal and answering it is the correct behaviour. A checker that refused
    this would be one people route around."""
    text = ('[dependency-groups]\n'
            'a = ["pysail==0.7.0"]\n'
            'b = ["pysail==0.7.0"]\n')
    assert t.version_of("pysail", text) == "0.7.0"


def test_a_non_numeric_version_is_not_read_as_a_pin():
    """The pattern requires a leading digit, so `pysail==@local` or a git marker
    is not mistaken for a version. Publishing a tag from one would name something
    no registry can resolve."""
    with pytest.raises(SystemExit):
        t.version_of("pysail", '[dependency-groups]\nsail = ["pysail==main"]\n')


# --- the live-repo control ----------------------------------------------------
#
# Two assertions against the real tree, because everything above is synthetic and
# a resolver that had stopped matching this repository's actual pin syntax would
# pass all of it.


def test_every_published_sidecar_image_is_in_the_map():
    """TAGGED_BY must cover every image release.yml publishes from the
    compute-images matrix. This is the direction that matters: an image added to
    that matrix with no entry here makes `image_tags.py <name>` print the whole
    map instead of one version, and the workflow interpolates that multi-line
    output into a tag. Derived from release.yml rather than transcribed, so the
    two cannot drift."""
    release = (REPO / ".github" / "workflows" / "release.yml").read_text(encoding="utf-8")
    block = release.split("compute-images:", 1)
    assert len(block) == 2, "release.yml has no compute-images job; update this test"
    # The matrix `include:` entries, up to the job's `steps:`.
    matrix = block[1].split("steps:", 1)[0]
    published = set(re.findall(r"^\s+- name: (\S+)$", matrix, re.MULTILINE))
    assert published, "parsed no image names out of the compute-images matrix"
    assert published <= set(t.TAGGED_BY), (
        f"release.yml publishes {sorted(published - set(t.TAGGED_BY))} with no "
        "entry in image_tags.TAGGED_BY, so the tag resolution for it prints the "
        "whole map rather than one version")


def test_the_real_pins_still_resolve():
    """The resolver against this repository's own pyproject.toml, with no
    assertion about the numbers. A pattern that had stopped matching the real pin
    syntax would satisfy every synthetic case above and fail here."""
    for image, package in t.TAGGED_BY.items():
        version = t.version_of(package)
        assert re.fullmatch(r"[0-9][0-9A-Za-z.\-+]*", version), \
            f"{image} resolved to {version!r}, which is not a usable tag"


# --- the CLI contract ---------------------------------------------------------
#
# `main` is what release.yml calls, and the way it calls it is what makes the
# output shape load bearing:
#
#     echo "version=$(python3 scripts/image_tags.py sail)" >> "$GITHUB_OUTPUT"
#
# A GITHUB_OUTPUT assignment is one line. Anything that prints two lines there
# writes a malformed output file, and the tag downstream is empty or garbage.


def test_naming_one_image_prints_exactly_its_version(capsys):
    """One line, no key, no decoration -- the workflow interpolates this straight
    into `version=`."""
    assert t.main(["image_tags.py", "sail"]) == 0
    out = capsys.readouterr().out
    assert out == t.version_of("pysail") + "\n"


def test_naming_no_image_prints_the_whole_map(capsys):
    """The human form: every image and its version, one per line. This is the
    shape a person reads, and it is deliberately NOT what the workflow asks
    for."""
    assert t.main(["image_tags.py"]) == 0
    lines = capsys.readouterr().out.strip().split("\n")
    assert len(lines) == len(t.TAGGED_BY)
    assert all("=" in line for line in lines)


def test_an_unknown_image_name_does_not_print_one_version(capsys):
    """The dangerous near-miss, because it fails QUIETLY in the place it matters.
    A misspelled or renamed matrix entry falls through to the whole-map branch, so
    the workflow's `$(...)` captures several lines and writes a broken
    GITHUB_OUTPUT rather than refusing. Pinned so the behaviour is at least known:
    the output is NOT a single version, which is what a caller would have to check
    for. The `test_every_published_sidecar_image_is_in_the_map` assertion above is
    what stops this being reachable from release.yml at all."""
    assert t.main(["image_tags.py", "saul"]) == 0
    out = capsys.readouterr().out
    assert len(out.strip().split("\n")) == len(t.TAGGED_BY)
    assert "saul" not in out
