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

permissions:
  contents: read

jobs:
  build:
    runs-on: ubuntu-latest
    timeout-minutes: 25
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


# --- second invariant: every job declares timeout-minutes --------------------
#
# GitHub's default is SIX HOURS, so an untimed job does not fail when it wedges
# — it holds a runner all afternoon and reads as "still pending" to anything
# watching. A Sail job did exactly that for 78 minutes while a merge watcher
# waited on it. The timeouts were added to all 47 jobs in one pass; nothing
# stopped the 48th arriving without one.

TIMED = """\
name: CI
on: [push]

concurrency:
  group: ci-${{ github.event_name == 'push' && github.sha || github.ref }}
  cancel-in-progress: ${{ github.event_name != 'push' }}

permissions:
  contents: read

jobs:
  build:
    runs-on: ubuntu-latest
    timeout-minutes: 25
  test:
    runs-on: ubuntu-latest
    timeout-minutes: 30
"""


def test_a_job_without_a_timeout_fails(workflows, capsys):
    workflows(ci=TIMED.replace("  test:\n    runs-on: ubuntu-latest\n    timeout-minutes: 30\n",
                               "  test:\n    runs-on: ubuntu-latest\n"))
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "timeout-minutes" in out and "test" in out, out
    assert "6-hour" in out, "the message must say what the default costs"


def test_every_job_timed_passes(workflows, capsys):
    workflows(ci=TIMED, mt=TIMED.replace("group: ci-", "group: make-targets-"))
    assert c.main() == 0
    assert "declare timeout-minutes" in capsys.readouterr().out


# A job that CALLS a reusable workflow cannot carry timeout-minutes — GitHub
# rejects the key. Demanding one there is a false positive, and a check that
# fires on something you cannot fix is a check somebody deletes. Found by
# running the first draft against the real repo: it flagged release.yml's two
# `uses:` jobs, which are exactly this case.
def test_a_reusable_workflow_call_is_exempt(workflows, capsys):
    workflows(ci=TIMED + """  call:
    uses: ./.github/workflows/make-targets.yml
""", mt=TIMED.replace("group: ci-", "group: make-targets-"))
    assert c.main() == 0, capsys.readouterr().out


# ...but the exemption must be narrow: `uses:` on a STEP is not a workflow call,
# and a job full of actions still needs its own timeout. Widening the rule to
# "any uses: anywhere" would silently exempt most of the matrix.
def test_a_step_level_uses_is_not_exempt(workflows, capsys):
    workflows(ci=TIMED + """  stepped:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
""", mt=TIMED.replace("group: ci-", "group: make-targets-"))
    assert c.main() == 1
    assert "stepped" in capsys.readouterr().out


# `on:` also carries two-space keys (push:, pull_request:). Counting those as
# jobs was the first draft's bug — it read 47 jobs where there were 45 and
# reported `push:` as missing a timeout.
def test_a_workflow_without_top_level_permissions_fails(workflows, capsys):
    # The third invariant had no test at all. It was added to the checker, the
    # baseline above did not carry a `permissions:` block, and four unrelated
    # tests went red -- which is how the suite reported it: as four failures
    # about concurrency and timeouts, none of them naming permissions. An
    # invariant nobody has watched fail is an invariant nobody knows the
    # direction of.
    workflows(ci=GOOD_CI.replace("permissions:\n  contents: read\n\n", ""))
    assert c.main() == 1
    out = capsys.readouterr().out
    assert "no top-level `permissions:`" in out
    assert "ci.yml" in out


def test_a_job_level_permissions_block_is_not_mistaken_for_the_workflows(workflows, capsys):
    # Indented, so it belongs to the job and leaves every OTHER job on the
    # repository default. The checker anchors at column 0 for exactly this.
    indented = GOOD_CI.replace(
        "permissions:\n  contents: read\n\n", ""
    ).replace(
        "    timeout-minutes: 25", "    timeout-minutes: 25\n    permissions:\n      contents: read"
    )
    workflows(ci=indented)
    assert c.main() == 1
    assert "no top-level `permissions:`" in capsys.readouterr().out


def test_on_block_keys_are_not_counted_as_jobs(workflows, capsys):
    workflows(ci="""\
name: CI
on:
  push:
    branches: [main]
  pull_request:

concurrency:
  group: ci-${{ github.event_name == 'push' && github.sha || github.ref }}
  cancel-in-progress: ${{ github.event_name != 'push' }}

permissions:
  contents: read

jobs:
  build:
    runs-on: ubuntu-latest
    timeout-minutes: 25
""", mt=TIMED.replace("group: ci-", "group: make-targets-"))
    assert c.main() == 0, capsys.readouterr().out
