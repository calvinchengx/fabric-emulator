#!/usr/bin/env python3
"""Which CI lane does this change need? Decides whether CI may skip work it cannot use.

Three lanes, and the safe direction between them is always toward `full`:

  docs    documentation only -- the gates that read prose run, and nothing else
  portal  documentation and/or the portal (its source, its committed dist, the
          pnpm workspace that builds it) -- the Go test matrix and the four
          jobs that can observe the portal run, and the e2e stacks do not
  full    anything else -- everything runs

THE PORTAL LANE, and why it is shaped like this. On 2026-10-25 roughly half the
pull requests -- bits-ui, jsdom, svelte, vitest, mermaid, playwright,
svelte-check, lucide -- changed nothing but `portal/` or the pnpm lockfile, and
each ran ~110 checks: Spark, medallion, OpenMetadata, Kafka, dbt, none of which
can see a Svelte component. The jobs that CAN see one were found by looking, not
guessed: `portal/dist` is EMBEDDED in the Go binary (portal/embed.go) and eight
Go test files read it, so the Go test matrix runs; `portal`, `portal-types` and
`portal-terminal` read `portal/` directly. Those are PORTAL_LANE_JOBS below, and
the test module asserts ci.yml gates exactly them on `lane != 'docs'` and every
other conditional job on `lane == 'full'`, so the set cannot drift from the
workflow.

`pnpm-lock.yaml`, `pnpm-workspace.yaml` and the root `package.json` belong to the
lane because the workspace is exactly `portal` and `website` -- the lockfile's
only importers -- and the root manifest holds build scripts and nothing else.

A deletion or rename is allowed in the portal lane where it is not in the docs
lane: every portal rebuild renames a hashed dist asset, and the docs lane's one
deletion hazard (a Go test asserting examples/README.md's links) is a Go test,
which this lane runs.

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
# `site/` used to hold a hand-written landing page served at the root. There is
# one hero now -- website/src/pages/index.astro, copied to both the root and
# the docs root by the assembler -- so it lives under `website/` like the rest
# of the site, and `scripts/build_landing_data.py` reads it there. The check
# that can observe a hero edit still runs in the workflow that builds it, and
# no job this lane skips can see the file.
DOC_TREES = ("docs/", "website/")

# Loose files that are documentation despite living at the root. Listed rather
# than matched by `*.md`, so a new root-level markdown file (a LICENSE-adjacent
# policy, a generated report something else consumes) does not silently join
# the set by virtue of its extension.
DOC_FILES = ("README.md", "AUDIT.md", "SECURITY.md")


def is_doc(path: str) -> bool:
    return path.startswith(DOC_TREES) or path in DOC_FILES


# The portal, and the pnpm workspace files that can only reach it or the site.
PORTAL_TREES = ("portal/",)
PORTAL_FILES = ("pnpm-lock.yaml", "pnpm-workspace.yaml", "package.json")

# Jobs in ci.yml that run in the portal lane, WITH the reason each can observe a
# portal change. Every other conditional job in ci.yml runs in `full` only.
PORTAL_LANE_JOBS = {
    "test": "portal/dist is embedded in the Go binary and eight Go test files read it",
    "portal": "builds portal/ from source, checks the committed dist, smokes it in a browser",
    "portal-types": "type-checks portal/ (svelte-check)",
    "portal-terminal": "drives the portal's terminal pane in a real browser",
}


def is_portal(path: str) -> bool:
    return path.startswith(PORTAL_TREES) or path in PORTAL_FILES


def lane(name_status: str) -> tuple[str, str]:
    """Return (lane, reason) for the output of `git diff --name-status`."""
    rows = [ln.split("\t") for ln in name_status.splitlines() if ln.strip()]
    if not rows:
        return "full", "no changed files could be determined; running everything"
    for row in rows:
        if len(row) < 2:
            return "full", f"unparseable diff row {row!r}; running everything"
    docs_only, why = classify(name_status)
    if docs_only:
        return "docs", why
    for row in rows:
        for path in row[1:]:
            if not (is_doc(path) or is_portal(path)):
                return "full", f"{path} is outside the docs and portal lanes"
    return "portal", (f"all {len(rows)} changed path(s) are documentation or the "
                      "portal; the Go tests and the portal jobs run")


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
    name_status = sys.stdin.read()
    docs_only, _ = classify(name_status)
    which, reason = lane(name_status)
    out = os.environ.get("GITHUB_OUTPUT")
    if out:
        with open(out, "a", encoding="utf-8") as fh:
            fh.write(f"docs_only={'true' if docs_only else 'false'}\n")
            fh.write(f"lane={which}\n")
    print(f"lane={which} docs_only={'true' if docs_only else 'false'} — {reason}")
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
        ("M\twebsite/src/pages/index.astro", True,
         "the hero, which serves both the root and the docs root from one source"),
        ("M\twebsite/src/pages/index.astro\nM\tscripts/build_landing_data.py", False,
         "the guard that reads the hero is code, so it disqualifies the lane"),
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
    lane_cases: list[tuple[str, str, str]] = [
        ("M\tdocs/01-quickstart.md", "docs", "docs stay docs"),
        ("M\tportal/src/App.svelte", "portal", "portal source"),
        ("M\tportal/package.json\nM\tpnpm-lock.yaml", "portal", "a portal dependency bump"),
        ("R100\tportal/dist/assets/index-a.js\tportal/dist/assets/index-b.js\nM\tportal/dist/index.html",
         "portal", "a dist rebuild renames its hashed asset"),
        ("D\tdocs/55-new.md", "portal",
         "a deleted page: its one hazard is a Go test, and this lane runs the Go tests"),
        ("M\tportal/src/App.svelte\nM\tinternal/store/bus.go", "full", "portal plus Go"),
        ("M\tportal/src/App.svelte\nM\t.github/workflows/ci.yml", "full", "a workflow"),
        ("M\tportalx/thing.ts", "full", "a prefix that only resembles the portal"),
        ("M\twebsite/package.json\nM\tpnpm-lock.yaml", "portal", "a site dependency bump"),
        ("", "full", "empty diff"),
        ("M", "full", "unparseable row"),
    ]
    bad = []
    for diff, want, label in cases:
        got, reason = classify(diff)
        if got != want:
            bad.append(f"  {label}: wanted docs_only={want}, got {got} ({reason})")
    for diff, want, label in lane_cases:
        got, reason = lane(diff)
        if got != want:
            bad.append(f"  {label}: wanted lane={want}, got {got} ({reason})")
    if bad:
        print("docs_only_change --self-test FAILED:\n" + "\n".join(bad))
        return 1
    print(f"docs_only_change --self-test: {len(cases) + len(lane_cases)} cases pass")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
