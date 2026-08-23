#!/usr/bin/env python3
"""Is this change confined to documentation? Decides whether CI may run its cheap lane.

WHY THIS EXISTS. A one-file edit to `website/scripts/sync-docs.mjs` (#363) ran
**95 checks** on the pull request and the same matrix again on main: three
operating systems of Go tests, every compose-backed conformance suite, dbt,
Terraform, the medallion pairs. None of them can observe a markdown file. The
cost is not only runner minutes — it is the interval between "docs fixed" and
"docs published", which is what makes a small correction feel expensive enough
to skip.

WHAT MAKES THIS SAFE, AND THE ONE CASE THAT IS NOT. The docs gates do not move:
`witnesses` (parity claims, sidebar, intra-doc links, conformance matrix) runs
on EVERY change, docs or code, because it reads both. What the docs lane skips
is only the code that cannot see a markdown file.

The exception is real and was found by looking rather than assumed:
`internal/api/examples_readme_test.go` reads `examples/README.md` and fails if
a `(../docs/<file>)` link points at a file that does not exist. So a docs
change CAN break a Go test — but only by REMOVING or RENAMING a page, never by
adding or editing one. That is why a delete or a rename anywhere in the change
disqualifies the cheap lane, whatever the paths say.

SAFE DIRECTION IS "RUN EVERYTHING". Every unknown answers `false`: an empty
diff, a range git cannot resolve, a force push whose `before` is the null sha,
a path outside the known set. A classifier that guesses "docs" when it does not
know converts a missing verdict into a green one, which is the failure this
repo keeps finding in its own gates (see docs/parity.md's witness discipline,
and check_witnesses.py on skips that no longer skip).

Usage:
    git diff --name-status BASE HEAD | docs_only_change.py
    docs_only_change.py --self-test

Writes `docs_only=true|false` to $GITHUB_OUTPUT when set, and always prints the
verdict with the reason for it. Exit status is 0 for a readable answer of
either kind; non-zero only when the SELF-TEST fails.
"""
import os
import sys

# Everything under these is documentation for this purpose. `website/` is the
# Astro site: its scripts, components and config exist only to render `docs/`.
# `site/` is the hand-written landing page that the same published site serves
# at its root: one self-contained HTML file, read by nothing but a browser and
# by `scripts/build_landing_data.py`, which docs-site.yml runs on every change
# under `site/**`. So the check that can observe a landing-page edit runs in
# the workflow that builds it, and no job this lane skips can see the file.
DOC_TREES = ("docs/", "website/", "site/")

# Loose files that are documentation despite living at the root. Listed rather
# than matched by `*.md`, so a new root-level markdown file (a LICENSE-adjacent
# policy, a generated report something else consumes) does not silently join
# the set by virtue of its extension.
DOC_FILES = ("README.md", "AUDIT.md", "SECURITY.md")


def is_doc(path: str) -> bool:
    return path.startswith(DOC_TREES) or path in DOC_FILES


def classify(name_status: str) -> tuple[bool, str]:
    """Return (docs_only, reason) for the output of `git diff --name-status`."""
    rows = [ln.split("\t") for ln in name_status.splitlines() if ln.strip()]
    if not rows:
        # No diff at all is not "nothing to test": it is a range this did not
        # resolve (a merge commit read the wrong way, a shallow checkout, a
        # tag). The full suite is the honest answer to a question we failed
        # to ask.
        return False, "no changed files could be determined; running everything"

    for row in rows:
        status, paths = row[0], row[1:]
        if not paths:
            return False, f"unparseable diff row {row!r}; running everything"
        # A rename reports both sides; both must be documentation, and it is
        # disqualifying anyway (below) — check the paths first so the message
        # names the more specific reason when a rename leaves the doc trees.
        for path in paths:
            if not is_doc(path):
                return False, f"{path} is not documentation"
        if status[:1] in ("D", "R"):
            verb = "deleted" if status[:1] == "D" else "renamed"
            return False, (
                f"{paths[0]} was {verb}; a page that disappears can break "
                "examples/README.md's links, which a Go test asserts")

    return True, f"all {len(rows)} changed path(s) are documentation"


def main(argv: list[str]) -> int:
    if "--self-test" in argv:
        return self_test()
    docs_only, reason = classify(sys.stdin.read())
    out = os.environ.get("GITHUB_OUTPUT")
    if out:
        with open(out, "a", encoding="utf-8") as fh:
            fh.write(f"docs_only={'true' if docs_only else 'false'}\n")
    print(f"docs_only={'true' if docs_only else 'false'} — {reason}")
    return 0


def self_test() -> int:
    """Runs in CI beside the real classification, so the gate proves itself.

    A path classifier is the kind of thing that keeps working while being
    wrong: `docs_only=false` is always safe and always plausible, so a rule
    that stopped matching anything would simply run the full suite forever and
    nobody would file a bug. These cases are the only thing that would notice.
    """
    cases: list[tuple[str, bool, str]] = [
        ("M\tdocs/01-quickstart.md", True, "edited page"),
        ("M\twebsite/scripts/sync-docs.mjs", True, "the #363 change itself"),
        ("M\tsite/index.html", True, "the landing page the docs site serves at its root"),
        ("M\tsite/index.html\nM\tscripts/build_landing_data.py", False,
         "the guard that reads the landing page is code, so it disqualifies the lane"),
        ("M\tREADME.md", True, "root readme"),
        ("A\tdocs/55-new.md\nM\tREADME.md", True, "several docs"),
        ("M\tinternal/api/items.go", False, "code"),
        ("M\tdocs/01-quickstart.md\nM\tinternal/api/items.go", False, "mixed"),
        ("M\t.github/workflows/ci.yml", False, "a workflow is not documentation"),
        ("M\texamples/README.md", False, "examples are executed, not read"),
        ("D\tdocs/55-new.md", False, "deletion can break a link a Go test asserts"),
        ("R100\tdocs/a.md\tdocs/b.md", False, "rename, same reason"),
        ("", False, "empty diff means unknown, not clean"),
        ("M", False, "unparseable row"),
    ]
    bad = []
    for diff, want, label in cases:
        got, reason = classify(diff)
        if got != want:
            bad.append(f"  {label}: wanted docs_only={want}, got {got} ({reason})")
    if bad:
        print("docs_only_change --self-test FAILED:\n" + "\n".join(bad))
        return 1
    print(f"docs_only_change --self-test: {len(cases)} cases pass")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
