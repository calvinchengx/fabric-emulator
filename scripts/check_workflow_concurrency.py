#!/usr/bin/env python3
"""ci.yml and make-targets.yml must group and cancel by the same rule.

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

Usage:
    check_workflow_concurrency.py     exit non-zero describing any divergence
"""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
WORKFLOWS = ROOT / ".github" / "workflows"
COUPLED = ("ci.yml", "make-targets.yml")

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

    if problems:
        print("check_workflow_concurrency: " + "\n\n".join(problems))
        print(f"\n{a} and {b} together are what \"main is green\" means; a verdict from "
              "one of them is half an answer.")
        return 1

    print(f"check_workflow_concurrency: {a} and {b} agree "
          f"(group {expr_a}, cancel-in-progress {cancel_a})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
