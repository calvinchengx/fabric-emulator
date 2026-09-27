"""Tests for the compute-sidecar digest pin checker.

THE INVARIANT IS HELD AS OF THIS COMMIT, which is the awkward position every
checker's test file in this repository is in: running it against the real tree
proves only that it did not crash, and a checker that has never rejected anything
is indistinguishable from one whose detection stopped matching. So every case
here drives it over a synthetic tree under tmp_path with a violation it MUST
catch, or a near-miss it must NOT.

THE NEAR-MISSES ARE WHERE THE VALUE IS, and this checker's own docstring records
why: its first cut scanned `.md` and flagged a README. A rule that demands a
64-hex digest inside prose produces documentation nobody can read and that goes
stale every release, so it gets muted, and a muted check is the failure mode
docs/10-testing.md calls item Eight. The three narrowings below -- by file
suffix, by an in-place exemption, and by the lookbehind that keeps the OLD image
name out -- are each asserted in both directions for that reason.

Never over this repository's own files: the real references carry real digests
that rotate on every publish, so a test reading them would assert today's hashes
and fail on the next legitimate release.
"""
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_image_digests as c  # noqa: E402

# A real digest's shape: sha256 and exactly 64 lowercase hex. The value is
# nonsense on purpose -- what is under test is the pin's PRESENCE.
DIGEST = "@sha256:" + "ab12" * 16


def write(root, rel, text):
    path = root / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")
    return path


def refs(root):
    """The checker's findings over a synthetic tree, as a list of (path, line).

    `as_posix()`, not `str()`: the checker yields Path objects, and `str()` of
    one on Windows uses backslashes, so a test written as `docker/.env` failed
    on windows-latest while passing everywhere else."""
    return [(p.as_posix(), n) for p, n, _ in c.offenders(root=root)]


# --- what must be REFUSED -----------------------------------------------------


def test_an_unpinned_tag_in_a_compose_file_is_reported(tmp_path):
    write(tmp_path, "docker-compose.yml",
          "services:\n  sail:\n    image: ghcr.io/x/emulator-sail:0.7.0\n")
    found = c.offenders(root=tmp_path)
    assert [(str(p), n) for p, n, _ in found] == [("docker-compose.yml", 3)]


def test_the_reported_reference_is_the_one_that_floats(tmp_path):
    """The report has to name the reference, not just the line: a compose file
    can hold several images on adjacent lines and "line 3 is wrong" sends the
    reader to whichever one they notice first."""
    write(tmp_path, "docker-compose.yml",
          "    image: ghcr.io/x/emulator-spark-agent:4.2.0\n")
    (_, _, ref), = c.offenders(root=tmp_path)
    assert ref == "emulator-spark-agent:4.2.0"


def test_the_env_file_compose_actually_reads_is_in_scope(tmp_path):
    """THE DEFECT THIS FILE FOUND, and the reason it is a regression test rather
    than a statement of the obvious. `.env` is in scope because compose
    interpolates it: the pull happens with whatever this says, so an unpinned
    value here floats the stack exactly as one written inline would. The checker
    listed ".env" in PULLING_SUFFIXES from the day it was written and that line
    never matched a single file -- `Path(".env").suffix` is `""`, because the
    leading dot makes the whole name a stem. So the canonical name, the one
    `env_file:` defaults to, was out of scope while the constant said it was in,
    and `examples/fab-driven/.env` is tracked in this repository and was never
    inspected.

    It could not have been found by reading either the checker or the tree: an
    unscanned file produces no finding and no error, so it reads exactly like a
    clean one. Only driving it over a file that MUST be reported says so."""
    write(tmp_path, "docker/.env", "SAIL_IMAGE=ghcr.io/x/emulator-sail:0.7.0\n")
    assert refs(tmp_path) == [("docker/.env", 1)]


def test_a_suffixed_env_file_is_in_scope_too(tmp_path):
    """The other half of the same scope, and the half that always worked:
    `sail.env` matches by suffix. Asserted so a future narrowing of the name
    match cannot quietly trade one for the other."""
    write(tmp_path, "docker/sail.env", "SAIL_IMAGE=ghcr.io/x/emulator-sail:0.7.0\n")
    assert refs(tmp_path) == [("docker/sail.env", 1)]


def test_every_unpinned_reference_on_one_line_is_reported(tmp_path):
    """Two images on one line is a real shape in a `docker run` invocation
    inside a workflow. Reporting the first and stopping would leave the second
    floating behind a green check."""
    write(tmp_path, "ci.yml",
          "run: docker pull ghcr.io/x/emulator-sail:0.7.0 ghcr.io/x/emulator-spark-agent:4.2.0\n")
    assert len(refs(tmp_path)) == 2


# --- what must be ALLOWED -----------------------------------------------------


def test_a_digest_pinned_reference_passes(tmp_path):
    write(tmp_path, "docker-compose.yml",
          f"    image: ghcr.io/x/emulator-sail:0.7.0{DIGEST}\n")
    assert refs(tmp_path) == []


def test_prose_naming_the_image_is_left_alone(tmp_path):
    """The narrowing this checker learned the hard way -- its first cut scanned
    `.md` and flagged a README. A sentence pulls nothing, and a 64-hex digest
    inside prose is unreadable and stale on every release."""
    write(tmp_path, "docs/20-sail.md",
          "The engine ships as `ghcr.io/x/emulator-sail:0.7.0`.\n")
    assert refs(tmp_path) == []


def test_an_exempt_line_is_skipped_and_the_reason_is_required(tmp_path):
    """A tag that floats ON PURPOSE (`:dev`, a locally built image) has no digest
    to pin and pinning one would defeat it. Both halves asserted, because
    "exemption too broad" and "exemption absent" look identical in a green log:
    the marker WITH a reason is honoured, and the bare marker is not."""
    write(tmp_path, "docker-compose.override.yml",
          "    image: ghcr.io/x/emulator-sail:dev  # digest-exempt: built locally\n")
    assert refs(tmp_path) == []

    write(tmp_path, "docker-compose.override.yml",
          "    image: ghcr.io/x/emulator-sail:dev  # digest-exempt:\n")
    assert refs(tmp_path) == [("docker-compose.override.yml", 1)], \
        "a bare `digest-exempt:` with no reason must not silence the finding"


def test_the_old_image_name_is_not_governed(tmp_path):
    """The lookbehind is load-bearing, and its own comment says so:
    `fabric-emulator-spark-agent` contains `emulator-spark-agent` as a suffix.
    Without the guard the checker reports the OLD name -- which release.yml still
    publishes under its release-version tag, so every existing reference to it
    would be a finding this rule was never about."""
    write(tmp_path, "docker-compose.yml",
          "    image: ghcr.io/o/fabric-emulator-spark-agent:0.27.0\n")
    assert refs(tmp_path) == []


def test_a_skipped_directory_is_not_walked(tmp_path):
    """A vendored or generated tree is somebody else's configuration, and drift
    reported inside one is the fastest way to teach a reader to skim past this
    check."""
    write(tmp_path, "node_modules/pkg/compose.yml",
          "    image: ghcr.io/x/emulator-sail:0.7.0\n")
    assert refs(tmp_path) == []


def test_a_build_service_has_no_tag_to_pin(tmp_path):
    """`build:` produces the image locally, so there is nothing published to
    name. Nothing in the file mentions the image at all, and the checker must not
    invent a requirement from the service name."""
    write(tmp_path, "docker-compose.yml",
          "services:\n  sail:\n    build: docker/sail\n")
    assert refs(tmp_path) == []


# --- the exit-code contract ---------------------------------------------------


def test_main_exits_non_zero_on_an_unpinned_reference(tmp_path, monkeypatch, capsys):
    """`main` is what the witnesses job runs, so its EXIT CODE is the actual
    contract -- a checker that finds the problem and returns 0 is not a gate.
    `offenders` is redirected at the synthetic tree rather than ROOT being
    patched, because `root=ROOT` is a default argument bound at definition and
    patching the global would not reach it."""
    write(tmp_path, "docker-compose.yml",
          "    image: ghcr.io/x/emulator-sail:0.7.0\n")
    real = c.offenders
    monkeypatch.setattr(c, "offenders", lambda: real(root=tmp_path))
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "emulator-sail:0.7.0" in out
    assert "docker-compose.yml:1" in out


def test_main_exits_zero_when_everything_is_pinned(tmp_path, monkeypatch, capsys):
    write(tmp_path, "docker-compose.yml",
          f"    image: ghcr.io/x/emulator-sail:0.7.0{DIGEST}\n")
    real = c.offenders
    monkeypatch.setattr(c, "offenders", lambda: real(root=tmp_path))
    assert c.main() == 0
    assert "pins @sha256" in capsys.readouterr().out


def test_the_governed_names_are_derived_from_the_tag_map():
    """The image names are not typed twice: image_tags.TAGGED_BY owns the suffix
    list, and a sidecar added there must come under this rule automatically. A
    second hand-written list is how one of them would quietly stop being
    governed."""
    import image_tags

    assert tuple(f"emulator-{s}" for s in image_tags.TAGGED_BY) == c.IMAGES
    assert c.IMAGES, "no images are governed at all, so this checker is vacuous"


def test_an_undecodable_file_is_skipped_rather_than_crashing(tmp_path):
    """A `.yml` that is not UTF-8 -- a stray binary, a latin-1 fixture -- must not
    take the sweep down. A crash here reads as a broken checker rather than as a
    bad file, and the run that crashes inspects nothing after it: the sweep would
    stop partway through with an exception and every reference below the bad file
    would go unexamined behind what looks like infrastructure noise."""
    (tmp_path / "bad.yml").write_bytes(b"\xff\xfe image: x\n")
    (tmp_path / "docker-compose.yml").write_text(
        "    image: ghcr.io/x/emulator-sail:0.7.0\n", encoding="utf-8")
    assert refs(tmp_path) == [("docker-compose.yml", 1)]
