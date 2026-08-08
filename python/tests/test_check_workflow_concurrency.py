"""Tests for the ci.yml / make-targets.yml concurrency coupling check.

The checker guards a coupling that is currently SATISFIED, which is the awkward
case to test: a check that passes today passes whether or not it works. So
every test here drives it with a divergence it must catch, and the real-repo
case is the single control at the end.

The divergences are the ones that actually change behaviour, not cosmetic
differences — a check that fired on whitespace would be turned off within a
week.
"""
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[2] / "scripts"))

import check_workflow_concurrency as c  # noqa: E402

GOOD_CI = """\
name: CI
on: [push]

concurrency:
  group: ci-${{ github.event_name == 'push' && github.sha || github.ref }}
  cancel-in-progress: ${{ github.event_name != 'push' }}

jobs:
  build:
    runs-on: ubuntu-latest
"""

GOOD_MT = GOOD_CI.replace("name: CI", "name: Make targets").replace("group: ci-", "group: make-targets-")


@pytest.fixture
def workflows(tmp_path, monkeypatch):
    """Write a pair of workflow files and point the checker at them."""
    def build(ci=GOOD_CI, mt=GOOD_MT):
        d = tmp_path / "workflows"
        d.mkdir(exist_ok=True)
        (d / "ci.yml").write_text(ci)
        (d / "make-targets.yml").write_text(mt)
        monkeypatch.setattr(c, "WORKFLOWS", d)
        return d
    return build


def test_passes_when_the_two_agree(workflows, capsys):
    workflows()
    assert c.main() == 0
    assert "agree" in capsys.readouterr().out


# --- the divergences that change behaviour -----------------------------------

def test_a_different_group_expression_fails(workflows, capsys):
    # The failure this exists to catch: one workflow groups by ref on main, so
    # a later commit cancels an earlier commit's run and that commit never gets
    # a verdict — while the other workflow still reports green.
    workflows(mt=GOOD_MT.replace(
        "${{ github.event_name == 'push' && github.sha || github.ref }}",
        "${{ github.ref }}"))
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "EXPRESSION differs" in out
    assert "github.sha" in out and "github.ref" in out, "the message must show both sides"


def test_a_different_cancel_policy_fails(workflows, capsys):
    workflows(mt=GOOD_MT.replace(
        "cancel-in-progress: ${{ github.event_name != 'push' }}",
        "cancel-in-progress: true"))
    assert c.main() == 1
    assert "cancel-in-progress differs" in capsys.readouterr().out


def test_an_identical_prefix_fails(workflows, capsys):
    # The INVERSE failure, and the reason the prefix is excluded from the
    # expression comparison rather than ignored: same prefix + same expression
    # means one shared group, so the two workflows cancel each other and only
    # one of them ever reports. Comparing expressions alone would pass this.
    workflows(mt=GOOD_MT.replace("group: make-targets-", "group: ci-"))
    assert c.main() == 1
    assert "share a group" in capsys.readouterr().out


# --- refusing to pass vacuously ----------------------------------------------

def test_a_missing_concurrency_block_fails(workflows, capsys):
    # A workflow that dropped its block is UNPROTECTED. Reading that as
    # "nothing to compare, therefore fine" is the permissive default that turns
    # every gap into a silent success.
    workflows(mt="name: Make targets\non: [push]\n\njobs:\n  build:\n    runs-on: ubuntu-latest\n")
    assert c.main() == 1
    assert "no top-level `concurrency:` block" in capsys.readouterr().out


def test_a_job_level_block_is_not_mistaken_for_the_workflows(workflows, capsys):
    # `concurrency:` is legal inside a job too. An indented one must not be
    # picked up as the workflow's, or a file with no workflow-level block would
    # be compared against a job's and could pass while unprotected.
    workflows(mt="""\
name: Make targets
on: [push]

jobs:
  build:
    runs-on: ubuntu-latest
    concurrency:
      group: make-targets-${{ github.event_name == 'push' && github.sha || github.ref }}
      cancel-in-progress: ${{ github.event_name != 'push' }}
""")
    assert c.main() == 1
    assert "no top-level `concurrency:` block" in capsys.readouterr().out


def test_a_constant_group_fails(workflows, capsys):
    # A constant group puts every run of that workflow in one bucket, so each
    # push cancels the last — including main commits that must each get a verdict.
    workflows(mt=GOOD_MT.replace(
        "group: make-targets-${{ github.event_name == 'push' && github.sha || github.ref }}",
        "group: make-targets"))
    assert c.main() == 1
    assert "constant" in capsys.readouterr().out


def test_a_missing_file_fails(tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(c, "WORKFLOWS", tmp_path / "nowhere")
    assert c.main() == 1
    assert "cannot read" in capsys.readouterr().out


# --- the control -------------------------------------------------------------

def test_the_real_repo_passes_its_own_check(capsys):
    assert c.main() == 0, capsys.readouterr().out
