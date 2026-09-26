"""Tests for the dependency-scanner coverage guard.

The awkward shape, and the same one test_check_workflow_concurrency.py faces:
this checker passes against the repository today, and a checker that passes
tells you nothing about whether it works. So the real repository is ONE control
at the top, and every other test drives the checker with a drift it must catch
-- asserting not merely that it fails, but that the message NAMES the offending
path or dependency. A checker failing for the wrong reason reads exactly like a
checker working.

The three drifts here are the three this repository has actually suffered,
recorded in .github/dependabot.yml's own comments: a manifest nothing watches
(seven example lockfiles), a watched directory that stopped existing
(examples/medallion, reorganised away in 48393313), and a hold whose
justification was temporary.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_dependency_risk as c  # noqa: E402

GOOD = """\
version: 2

updates:
  - package-ecosystem: gomod
    directory: /
    schedule:
      interval: weekly

  - package-ecosystem: uv
    directories:
      - /
      # A glob, so a new example is covered by existing.
      - /examples/*
    schedule:
      interval: weekly
    ignore:
      # mlflow 3.15.1 `requires_dist`: `pandas<3`.
      #
      # LIFT THIS when mlflow admits pandas 3.
      - dependency-name: pandas
        update-types: ['version-update:semver-major']

  - package-ecosystem: npm
    directory: /
    schedule:
      interval: weekly

  - package-ecosystem: github-actions
    directory: /
    schedule:
      interval: weekly
"""

FILES = (
    "go.mod",
    "uv.lock",
    "package.json",
    "pnpm-lock.yaml",
    "pnpm-workspace.yaml",
    "portal/package.json",
    "examples/alpha/uv.lock",
    "examples/beta/uv.lock",
)

WORKSPACE = "packages:\n  - portal\n"


@pytest.fixture
def repo(tmp_path, monkeypatch):
    """Build a small tree and point the checker at it instead of this repo."""
    def build(config=GOOD, files=FILES, excluded=()):
        (tmp_path / ".github" / "workflows").mkdir(parents=True, exist_ok=True)
        (tmp_path / ".github" / "dependabot.yml").write_text(config, encoding="utf-8")
        for rel in files:
            path = tmp_path / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(WORKSPACE if rel == "pnpm-workspace.yaml" else "",
                            encoding="utf-8")
        monkeypatch.setattr(c, "ROOT", tmp_path)
        monkeypatch.setattr(c, "CONFIG", tmp_path / ".github" / "dependabot.yml")
        monkeypatch.setattr(c, "tracked_files", lambda root: list(files))
        monkeypatch.setattr(c, "EXCLUDED", tuple(excluded))
        return tmp_path
    return build


# --- the controls ------------------------------------------------------------

def test_this_repository_passes_under_strict(capsys):
    """The one test that runs against the real tree, and the one that keeps the
    deliberate exclusions honest: --strict fails an exclusion that has stopped
    excusing anything, so a pattern left behind by a deleted harness cannot sit
    there hiding the next real omission."""
    assert c.main(["--strict"]) == 0, capsys.readouterr().err
    assert "tracked manifests" in capsys.readouterr().out


def test_the_fixture_tree_passes(repo, capsys):
    repo()
    assert c.main([]) == 0, capsys.readouterr().err


# --- INVARIANT 1: every manifest is covered by some entry ---------------------

def test_a_manifest_no_entry_covers_fails(repo, capsys):
    """The failure dependabot.yml's own comments record twice. `tools/` is
    outside every declared directory and every glob, so nothing scans it --
    and the scanners still report clean, which is why this is worse than
    having no scanner at all."""
    repo(files=(*FILES, "tools/uv.lock"))
    assert c.main([]) == 1
    err = capsys.readouterr().err
    assert "tools/uv.lock" in err, "the message must name the unwatched manifest"
    assert "uv" in err


def test_an_unwatched_dockerfile_fails(repo, capsys):
    """docker/sail and docker/spark-agent are PUBLISHED to GHCR and were among
    the eleven unwatched Dockerfiles. The fixture config declares no docker
    ecosystem at all, which is that state exactly."""
    repo(files=(*FILES, "docker/sail/Dockerfile"))
    assert c.main([]) == 1
    err = capsys.readouterr().err
    assert "docker/sail/Dockerfile" in err
    assert "docker" in err


def test_a_deliberate_exclusion_is_not_reported(repo, capsys):
    """An unwatched manifest is a failure OR a decision, and the difference has
    to be written down. Silence is not available."""
    repo(files=(*FILES, "e2e/harness/Dockerfile"),
         excluded=(("e2e/*/Dockerfile*", "harness image, never published"),))
    assert c.main([]) == 0, capsys.readouterr().err


def test_an_exclusion_does_not_cross_a_path_separator(repo, capsys):
    """`e2e/*/Dockerfile*` says how deep it reaches. Plain fnmatch would let it
    swallow e2e/sempy/image/Dockerfile as well, so a pattern would cover a
    depth it never mentions."""
    repo(files=(*FILES, "e2e/sempy/image/Dockerfile"),
         excluded=(("e2e/*/Dockerfile*", "harness image, never published"),))
    assert c.main([]) == 1
    assert "e2e/sempy/image/Dockerfile" in capsys.readouterr().err


def test_a_pnpm_workspace_member_resolves_to_the_root_lockfile(repo, capsys):
    """portal/package.json has no lockfile of its own, so the npm entry at `/`
    genuinely covers it. Reporting it unwatched would be a false positive, and
    a checker that cries wolf is a checker someone deletes."""
    repo()
    assert c.main([]) == 0, capsys.readouterr().err


def test_a_package_outside_the_workspace_is_not_resolved_away(repo, capsys):
    """The resolution above is a fact about pnpm-workspace.yaml, not a blanket
    pass for anything named package.json. Without the workspace file there is
    no root lockfile covering it, and it must be reported."""
    repo(files=tuple(f for f in FILES if f != "pnpm-workspace.yaml"))
    assert c.main([]) == 1
    assert "portal/package.json" in capsys.readouterr().err


# --- INVARIANT 2: every watched directory still exists -----------------------

def test_a_watched_directory_that_does_not_exist_fails(repo, capsys):
    """examples/medallion, exactly: reorganised away, still listed, and the
    only symptom was a job failing at file fetching where nobody reads."""
    repo(config=GOOD.replace("      - /examples/*\n",
                             "      - /examples/medallion\n"),
         files=tuple(f for f in FILES if not f.startswith("examples/")))
    assert c.main([]) == 1
    err = capsys.readouterr().err
    assert "/examples/medallion" in err, "the message must name the dead path"
    assert "NO directory" in err


def test_a_watched_directory_with_no_manifest_fails(repo, capsys):
    """A directory can survive a reorganisation while its manifest does not,
    and Dependabot fails the same way: `Repo must contain a requirements.txt,
    uv.lock, ...`."""
    tree = repo(config=GOOD.replace("      - /examples/*\n",
                                    "      - /examples/alpha\n"),
                files=tuple(f for f in FILES if f != "examples/alpha/uv.lock"))
    (tree / "examples" / "alpha").mkdir(parents=True, exist_ok=True)
    assert c.main([]) == 1
    err = capsys.readouterr().err
    assert "/examples/alpha" in err
    assert "nothing there" in err


def test_a_glob_matching_nothing_fails(repo, capsys):
    repo(config=GOOD.replace("      - /examples/*\n", "      - /samples/*\n"),
         files=tuple(f for f in FILES if not f.startswith("examples/")))
    assert c.main([]) == 1
    assert "/samples/*" in capsys.readouterr().err


# --- INVARIANT 3: every hold declares its exit condition ---------------------

def test_a_hold_with_no_exit_condition_fails(repo, capsys):
    """The rule check_dismissed_advisories.py already enforces for dismissed
    alerts: a hold justified by a temporary fact becomes permanent on the
    strength of that fact unless something re-asks the question."""
    repo(config=GOOD.replace("      # LIFT THIS when mlflow admits pandas 3.\n", ""))
    assert c.main([]) == 1
    err = capsys.readouterr().err
    assert "'pandas'" in err, "the message must name the dependency being held"
    assert "LIFT THIS" in err and "PERMANENT HOLD" in err


def test_a_permanent_marker_satisfies_the_invariant(repo, capsys):
    """apache/spark's case: its tag is a fidelity claim about Runtime 1.3, so
    there is no upstream change to wait for. Policy is a legitimate answer --
    it just has to be given."""
    repo(config=GOOD.replace("      # LIFT THIS when mlflow admits pandas 3.\n",
                             "      # PERMANENT HOLD: this pin IS the claim.\n"))
    assert c.main([]) == 0, capsys.readouterr().err


def test_a_justification_detached_from_its_hold_does_not_count(repo, capsys):
    """The comment run must be the one directly above the `dependency-name:`.
    A justification sitting above `ignore:` -- which is where apache/spark's
    was, and why this check found it -- explains the block, not the hold."""
    repo(config=GOOD.replace(
        "      # mlflow 3.15.1 `requires_dist`: `pandas<3`.\n"
        "      #\n"
        "      # LIFT THIS when mlflow admits pandas 3.\n"
        "      - dependency-name: pandas\n",
        "      - dependency-name: pandas\n").replace(
        "    ignore:\n",
        "    # LIFT THIS when mlflow admits pandas 3.\n    ignore:\n"))
    assert c.main([]) == 1
    assert "'pandas'" in capsys.readouterr().err


# --- the guard holds itself to account ---------------------------------------

def test_a_stale_exclusion_fails_under_strict(repo, capsys):
    """An exclusion excusing nothing is not harmless: it is a pattern that will
    quietly swallow the next manifest that happens to match it."""
    repo(excluded=(("e2e/*/Dockerfile*", "harness image, never published"),))
    assert c.main([]) == 0, capsys.readouterr().err
    repo(excluded=(("e2e/*/Dockerfile*", "harness image, never published"),))
    assert c.main(["--strict"]) == 1
    assert "e2e/*/Dockerfile*" in capsys.readouterr().err


def test_a_config_that_parses_to_nothing_fails(repo, capsys):
    """A checker that inspects nothing passes vacuously, which is the failure
    this whole file exists to prevent, one level up."""
    repo(config="version: 2\nupdates: []\n")
    assert c.main([]) == 1
    assert "zero `package-ecosystem` entries" in capsys.readouterr().err


def test_the_real_config_is_parsed_at_all():
    """Guards the regexes against the same vacuity: a parser that silently
    matched nothing would make every assertion above pass against this repo."""
    entries = c.read_config(c.CONFIG)
    assert {e["ecosystem"] for e in entries} == {
        "gomod", "npm", "uv", "docker", "github-actions"}
    assert sum(len(e["ignores"]) for e in entries) >= 6
    assert sum(len(e["directories"]) for e in entries) >= 7
