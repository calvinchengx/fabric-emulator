#!/usr/bin/env python3
"""Workflow invariants nothing else enforces: concurrency coupling, and job timeouts.

WHY THIS EXISTS. Together these two workflows are what "main is green" means —
a verdict from one of them is half an answer. Their `concurrency:` blocks decide
which runs supersede which: a PR ref's in-flight run is cancelled when the ref
moves, while every main commit gets its own group so every commit gets a
verdict. If one workflow drifts, it cancels a run the other protects, and the
release gate's behaviour changes with nothing failing to say so.

The coupling is currently satisfied and is stated only in a comment —
"Kept in step with ci.yml deliberately". A comment is not enforcement, and
neither is a test that names the right failure and measures next to it: the
docs-sidebar checker kept a copied regex under a comment AND a test asserting
that the string "DOC_RE" still appeared in the file it was copied from, which
stayed green through any edit to the pattern itself.

WHY A CHECKER RATHER THAN DERIVING OR COLLAPSING. Those are the better fixes
where they are available, and here neither is:

  * DERIVE is impossible — GitHub Actions has no cross-file include, and YAML
    anchors do not span documents, so neither expression can be computed from
    the other.
  * COLLAPSE is impossible — every workflow file must carry its own
    `concurrency:` block. The platform forces this one question to be asked
    twice.

So this is the residual case: two genuinely independent artifacts that must
merely agree. That is what a checker is for, and reaching for one when derive
or collapse was available would be the mistake — a checker is a second thing to
maintain and it only reports drift after the fact.

WHAT IS COMPARED. The `${{ … }}` expression of `group:` (the literal prefix
before it MUST differ, or the two workflows would share a group and cancel each
other) and the whole of `cancel-in-progress:`.

SECOND INVARIANT: EVERY JOB DECLARES `timeout-minutes`. GitHub's default is
**six hours**, so a job without one does not fail when it wedges — it occupies
a runner until the afternoon and reads as "still pending" to anything watching.
That is not hypothetical here: a Sail job sat `in_progress` for 78 minutes while
a merge watcher waited on it, because "running" and "wedged" are the same status
to `gh pr checks`. The timeouts were added to all 47 jobs in one pass; nothing
stopped the 48th from arriving without one, which is the same
maintained-by-attention coupling this file already exists to remove.

THIRD INVARIANT: EVERY WORKFLOW DECLARES A TOP-LEVEL `permissions:` BLOCK.
Without one a job gets whatever the repository default grants, which is a
write-scoped token for a job that only reads. CodeQL's `actions` pack found the
one file that lacked it -- codeql.yml, whose `analyze` job carried its own block
while its `changes` job carried none and therefore inherited the default. Every
other workflow already declared one, which is precisely the shape the paragraph
above describes: an invariant established in one pass, with nothing to stop the
next arrival from missing it. Top-level rather than per-job, so a job added
tomorrow inherits a floor instead of the default; a job needing more still
overrides it, as `analyze` does for `security-events: write`.

Usage:
    check_workflow_concurrency.py     exit non-zero describing any divergence
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
WORKFLOWS = ROOT / ".github" / "workflows"
COUPLED = ("ci.yml", "make-targets.yml")

# A top-level `permissions:` block. Anchored at column 0 for the same reason the
# concurrency pattern is: a job-level block is indented, and matching one would
# report a floor that does not exist.
_PERMISSIONS = re.compile(r"^permissions:\s*$", re.MULTILINE)

# A top-level `concurrency:` block and its two keys. Anchored at column 0 so a
# job-level concurrency block (indented) is never mistaken for the workflow's.
_BLOCK = re.compile(
    r"^concurrency:\s*$\n"
    r"(?P<body>(?:^[ \t]+.*$\n?)+)",
    re.M,
)
_GROUP = re.compile(r"^\s*group:\s*(?P<value>.+?)\s*$", re.M)
_CANCEL = re.compile(r"^\s*cancel-in-progress:\s*(?P<value>.+?)\s*$", re.M)
# The prefix that must differ, and the expression that must not.
_EXPR = re.compile(r"\$\{\{.*\}\}")


class ConcurrencyUnreadable(RuntimeError):
    """A workflow's concurrency block is absent or unparseable."""


def read_concurrency(path):
    """Return (group_prefix, group_expression, cancel_in_progress).

    Raises rather than returning a default: a workflow whose block cannot be
    read must fail the check, not silently agree with everything.
    """
    try:
        source = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ConcurrencyUnreadable(f"cannot read {path}: {exc}") from exc
    block = _BLOCK.search(source)
    if not block:
        raise ConcurrencyUnreadable(
            f"{path.name} has no top-level `concurrency:` block — the two workflows "
            "can no longer be compared, and one of them is now unprotected")
    body = block.group("body")
    group = _GROUP.search(body)
    cancel = _CANCEL.search(body)
    if not group or not cancel:
        raise ConcurrencyUnreadable(
            f"{path.name}'s concurrency block is missing `group:` or `cancel-in-progress:`")
    expr = _EXPR.search(group.group("value"))
    if not expr:
        raise ConcurrencyUnreadable(
            f"{path.name}'s concurrency group is a constant ({group.group('value')!r}); "
            "it must be an expression, or every run of that workflow shares one group")
    prefix = group.group("value")[: expr.start()]
    return prefix, expr.group(0), cancel.group("value")


# A job key: exactly two-space indent, inside the top-level `jobs:` block.
# Anchored that way because `on:` also carries two-space keys (`push:`,
# `pull_request:`) — counting those was the first draft's bug, and it inflated
# 45 jobs to 47 while reporting `push:` as missing a timeout.
_JOBS_START = re.compile(r"^jobs:\s*$", re.M)
_JOB_KEY = re.compile(r"^  ([A-Za-z0-9_-]+):\s*$")


def jobs_of(text):
    """[(job name, its body lines)] for every job in a workflow."""
    out, in_jobs = [], False
    for line in text.split("\n"):
        if _JOBS_START.match(line):
            in_jobs = True
            continue
        if in_jobs and line[:1].strip():  # a new top-level key ends the block
            break
        if in_jobs:
            match = _JOB_KEY.match(line)
            if match:
                out.append((match.group(1), []))
            elif out:
                out[-1][1].append(line)
    return out


def jobs_missing_timeout(path):
    """Job names in path that declare no `timeout-minutes`."""
    text = path.read_text(encoding="utf-8")
    found = jobs_of(text)
    if not found:
        raise ConcurrencyUnreadable(
            f"{path.name} parsed to zero jobs — a check that inspects nothing "
            "passes vacuously, which is worse than no check")
    missing = []
    for name, body in found:
        # A job that CALLS a reusable workflow (`uses:` at job level) cannot
        # carry `timeout-minutes` — GitHub rejects the key there. Its timeout
        # lives in the called workflow's own jobs, and since this check globs
        # every workflow in the directory, those jobs ARE covered: coverage is
        # preserved, not waived. Demanding a key the platform refuses is how a
        # check gets switched off, which costs more than the check was worth.
        if any(re.match(r"^    uses:\s", line) for line in body):
            continue
        if not any("timeout-minutes:" in line for line in body):
            missing.append(name)
    return missing


def main():
    try:
        read = {name: read_concurrency(WORKFLOWS / name) for name in COUPLED}
    except ConcurrencyUnreadable as exc:
        print(f"check_workflow_concurrency: {exc}")
        return 1

    a, b = COUPLED
    prefix_a, expr_a, cancel_a = read[a]
    prefix_b, expr_b, cancel_b = read[b]
    problems = []

    if expr_a != expr_b:
        problems.append(
            f"concurrency group EXPRESSION differs — one workflow will cancel a run the "
            f"other protects:\n    {a}: {expr_a}\n    {b}: {expr_b}")
    if cancel_a != cancel_b:
        problems.append(
            f"cancel-in-progress differs:\n    {a}: {cancel_a}\n    {b}: {cancel_b}")
    # The inverse invariant, and the reason the prefix is excluded above rather
    # than ignored: identical prefixes would put both workflows in ONE group, so
    # each new run would cancel the other's — the opposite failure, and one that
    # comparing only the expressions would happily wave through.
    if prefix_a.strip() == prefix_b.strip():
        problems.append(
            f"both workflows use the concurrency group prefix {prefix_a.strip()!r}; they "
            "would share a group and cancel each other's runs")

    # Every job in every workflow, not only the coupled pair — an untimed job
    # is a wedged runner wherever it lives.
    try:
        for path in sorted((WORKFLOWS).glob("*.yml")):
            missing = jobs_missing_timeout(path)
            if missing:
                problems.append(
                    f"{path.name}: {len(missing)} job(s) declare no `timeout-minutes`, so they "
                    f"inherit GitHub's 6-hour default and a wedged run reads as 'still pending':\n    "
                    + "\n    ".join(missing))
    except ConcurrencyUnreadable as exc:
        problems.append(str(exc))

    # A workflow with no floor hands every job in it the repository default.
    unscoped = [path.name for path in sorted(WORKFLOWS.glob("*.yml"))
                if not _PERMISSIONS.search(path.read_text(encoding="utf-8"))]
    if unscoped:
        problems.append(
            f"{len(unscoped)} workflow(s) declare no top-level `permissions:`, so every "
            f"job in them takes the repository default rather than a floor:\n    "
            + "\n    ".join(unscoped))

    if problems:
        print("check_workflow_concurrency: " + "\n\n".join(problems))
        print(f"\n{a} and {b} together are what \"main is green\" means; a verdict from "
              "one of them is half an answer.")
        return 1

    total = sum(len(jobs_of((WORKFLOWS / p.name).read_text(encoding="utf-8")))
                for p in sorted(WORKFLOWS.glob("*.yml")))
    print(f"check_workflow_concurrency: {a} and {b} agree "
          f"(group {expr_a}, cancel-in-progress {cancel_a}); "
          f"all {total} jobs declare timeout-minutes; "
          f"all {len(list(WORKFLOWS.glob('*.yml')))} workflows declare top-level permissions")
    return 0


if __name__ == "__main__":
    sys.exit(main())
