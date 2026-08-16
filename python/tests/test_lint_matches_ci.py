"""`make lint` must run the same checks as the CI lint job.

WHY. The Makefile target documents itself as "the CI lint job, locally", and
that claim was maintained by hand in two files that nothing compared. It had
already drifted once, in the direction that matters least visibly: CI gained a
dependency group the Makefile did not, so a developer running `make lint` got a
WEAKER check than the branch would get, and "it passed locally" stopped meaning
what people assume it means.

A weaker local check is worse than no local check. No local check sends you to
CI knowing you have not been told anything; a local check that silently omits a
group tells you that you have.

This compares the dependency groups the two invocations request, not the whole
command line, because they legitimately differ elsewhere: the Makefile wraps
both tools in a `command -v uv` guard and splits ruff and ty across two lines,
while CI runs each as its own step.
"""
import pathlib
import re

ROOT = pathlib.Path(__file__).resolve().parents[2]
MAKEFILE = ROOT / "Makefile"
CI = ROOT / ".github" / "workflows" / "ci.yml"


def _groups(command: str) -> set[str]:
    """The `--group X` flags in one shell command."""
    return set(re.findall(r"--group\s+([A-Za-z0-9._-]+)", command))


def _ty_invocations(text: str) -> list[str]:
    """Every line invoking `ty check`, whole-line so the flags come with it."""
    return [ln for ln in text.split("\n") if "ty check" in ln and "uv run" in ln]


def test_both_files_invoke_ty_exactly_once():
    """Two invocations in one file would make 'the groups agree' ambiguous, and
    zero would make every assertion below vacuously true."""
    for path in (MAKEFILE, CI):
        found = _ty_invocations(path.read_text(encoding="utf-8"))
        assert len(found) == 1, (
            f"{path.name} has {len(found)} `ty check` invocations, expected 1: {found}")


def test_make_lint_requests_the_same_groups_as_ci():
    make = _ty_invocations(MAKEFILE.read_text(encoding="utf-8"))[0]
    ci = _ty_invocations(CI.read_text(encoding="utf-8"))[0]

    make_groups, ci_groups = _groups(make), _groups(ci)
    assert make_groups, f"no --group flags parsed from the Makefile line: {make!r}"

    missing = ci_groups - make_groups
    extra = make_groups - ci_groups
    assert not missing, (
        f"`make lint` is WEAKER than CI: it omits {sorted(missing)}. A developer "
        f"running it would be told the types are fine when CI has not agreed.")
    assert not extra, (
        f"`make lint` requests {sorted(extra)} that CI does not, so it can fail "
        f"on something the branch will never be judged on.")


def test_the_agent_is_type_checked_at_all():
    """The specific group this test was written for.

    Without a group carrying pyspark, `import pyspark` resolves to nothing and
    every type in the statement agent is Unknown — ty runs, reports success, and
    has checked almost none of it. Asserting the group by name is crude, but the
    alternative is a check that passes while the thing it names is absent, which
    is the exact failure this file exists to stop.
    """
    ci = _ty_invocations(CI.read_text(encoding="utf-8"))[0]
    assert "spark-client" in _groups(ci), (
        "the CI ty job no longer installs a group carrying pyspark, so the "
        "statement agent is unchecked again")
