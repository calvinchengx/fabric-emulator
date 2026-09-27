#!/usr/bin/env python3
"""The dependency scanners still watch the repository that is actually here.

WHY THIS EXISTS. This repository is not short of dependency scanning.
`security.yml` runs gitleaks over the tree AND the history, `govulncheck ./...`
over the Go module with reachability filtering, and
`scripts/check_dismissed_advisories.py` to re-ask whether a dismissal has
expired. `.github/dependabot.yml` declares five ecosystems. What nothing
checked is whether any of that configuration still MATCHES the tree, and
`dependabot.yml`'s own comments record that failing twice:

  * **Seven example `uv.lock` files were watched by nothing.** They are not
    workspace members, so the root lockfile says nothing about them, and their
    pins had been drifting unobserved. These are the files people copy out of
    this repository to start with.
  * **Eleven of twelve Dockerfiles were unwatched**, including `docker/sail`
    and `docker/spark-agent` -- both PUBLISHED to GHCR and pulled family-wide,
    both `FROM python:3.12-slim` with nothing tracking the base.

Neither was found by looking. Both surfaced sideways, through a stale alert
(#67, tornado) naming `examples/medallion/uv.lock` -- a path deleted months
earlier in 48393313, which Dependabot kept trying to fetch and failing on with
`Repo must contain a requirements.txt, uv.lock, ...` every single run.

**A SCANNER THAT HAS STOPPED MATCHING THE TREE REPORTS CLEAN ON EXACTLY THE
MANIFESTS NOBODY IS WATCHING.** That is worse than no scanner, because it
produces a green check. Adding a sixth scanner would not help; asking whether
the five already here are still pointed at the repository is what was missing.

THE THREE INVARIANTS.

1. **EVERY TRACKED MANIFEST IS COVERED BY SOME ENTRY.** Every `go.mod`,
   `uv.lock`, `package.json` and `Dockerfile*` that `git ls-files` reports maps
   to the ecosystem Dependabot would use for it, and some entry's `directory`
   or `directories` glob must resolve to the directory it sits in. pnpm
   workspace members (`portal`, `website`) resolve to the root lockfile that
   genuinely covers them. Deliberate exclusions are DATA WITH A REASON below,
   never silence: an unlisted manifest fails, and a knowingly-unwatched one
   reads as a decision someone made.

2. **EVERY WATCHED DIRECTORY STILL EXISTS, AND STILL HOLDS A MANIFEST.** The
   `examples/medallion` failure, directly. A directory that was reorganised
   away does not make Dependabot complain anywhere a person reads; it makes the
   job fail at file fetching, every run, while the alert naming the deleted
   path outlives the path.

3. **EVERY `ignore:` HOLD DECLARES ITS EXIT CONDITION.** Each
   `dependency-name:` must carry, in the comment run directly above it, either
   `LIFT THIS` naming the upstream change that retires the hold, or
   `PERMANENT HOLD` for a hold that is policy rather than delay. This is the
   rule `check_dismissed_advisories.py` already enforces one file over, for the
   same reason: a hold justified by a temporary fact -- cryptography capped by
   mlflow's `requires_dist` -- becomes a permanent decision on the strength of
   that fact unless something re-asks the question. `apache/spark` is the
   genuinely permanent case and must say so, because its tag is a fidelity
   claim about Runtime 1.3 rather than a dependency to keep current.

OFFLINE AND STDLIB-ONLY, like its neighbours: `make check` is deliberately
runnable with nothing but Python, so `.github/dependabot.yml` is read with
regexes rather than PyYAML. The file list comes from `git ls-files` rather
than a filesystem walk, which keeps `node_modules/`, `.venv/`, `third_party/`,
`_site/` and `.claude/worktrees/` out without a hand-maintained deny list.

Usage:
    check_dependency_risk.py            exit non-zero naming the drift
    check_dependency_risk.py --strict   ...and refuse to pass vacuously
"""
from __future__ import annotations

import argparse
import fnmatch
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
CONFIG = ROOT / ".github" / "dependabot.yml"

# A hold is temporary or it is policy. Either is fine; an undeclared one is not.
EXIT_MARKERS = ("LIFT THIS", "PERMANENT HOLD")

# Manifests that are DELIBERATELY unwatched, each with its reason, so the
# decision is readable at the place the invariant is enforced. `*` does not
# cross a path separator here (see `matches`), so a pattern states how deep it
# reaches instead of quietly covering more than it says.
EXCLUDED: tuple[tuple[str, str], ...] = (
    (
        "e2e/*/Dockerfile*",
        "e2e harness image: built inside a CI run, never published and never "
        "reachable, so a base bump here competes for the same PR budget as the "
        "images that ship. Stated in .github/dependabot.yml's docker entry, "
        "which also says to add /e2e/* there if that judgement changes",
    ),
    (
        "e2e/*/*/Dockerfile*",
        "the same judgement for e2e/sempy/image, which sits one directory "
        "deeper than its siblings",
    ),
    (
        "examples/fab-driven/Dockerfile",
        "built by e2e/fab-driven/run.py and published nowhere -- named "
        "explicitly beside the e2e images in .github/dependabot.yml",
    ),
)


def matches(pattern: str, path: str) -> bool:
    """fnmatch, except `*` does not cross a path separator.

    Plain fnmatch would let `e2e/*/Dockerfile*` swallow
    `e2e/sempy/image/Dockerfile`, so one pattern would cover a depth it never
    mentions and the deeper exclusion would read as redundant when it is not.
    """
    left, right = pattern.split("/"), path.split("/")
    return len(left) == len(right) and all(
        fnmatch.fnmatch(b, a) for a, b in zip(left, right, strict=True)
    )


def tracked_files(root: pathlib.Path) -> list[str]:
    """Repo-relative paths git knows about."""
    out = subprocess.run(
        ["git", "-C", str(root), "ls-files"],
        check=True, capture_output=True, text=True,
    ).stdout
    return [line for line in out.split("\n") if line]


def ecosystem_of(path: str) -> str | None:
    """The `package-ecosystem` Dependabot would use for this manifest."""
    name = path.rsplit("/", 1)[-1]
    if name == "go.mod":
        return "gomod"
    if name == "uv.lock":
        return "uv"
    if name == "package.json":
        return "npm"
    if name == "Dockerfile" or name.startswith("Dockerfile."):
        return "docker"
    return None


def directory_of(path: str) -> str:
    """The repo-relative directory a manifest sits in; empty for the root."""
    return path.rsplit("/", 1)[0] if "/" in path else ""


def pnpm_members(root: pathlib.Path) -> set[str]:
    """Workspace packages whose dependencies resolve through the ROOT lockfile.

    `portal/package.json` and `website/package.json` have no lockfile of their
    own -- `pnpm-workspace.yaml` folds them into the root `pnpm-lock.yaml`,
    which is why one npm entry at `/` genuinely covers all three. Read from the
    workspace file rather than hard-coded, so adding a package to the workspace
    does not quietly need an edit here as well.
    """
    path = root / "pnpm-workspace.yaml"
    if not path.is_file() or not (root / "pnpm-lock.yaml").is_file():
        return set()
    members, in_packages = set(), False
    for line in path.read_text(encoding="utf-8").split("\n"):
        if re.match(r"^packages:\s*$", line):
            in_packages = True
            continue
        if in_packages:
            item = re.match(r"^\s+-\s*['\"]?(?P<value>[^'\"\s#]+)", line)
            if item:
                members.add(item.group("value").strip("/"))
            elif line.strip() and not line.startswith((" ", "\t", "#")):
                break
    return members


# --- reading .github/dependabot.yml ------------------------------------------
#
# Indent-anchored, the way check_workflow_concurrency.py anchors its blocks at
# column 0: an `updates:` entry key sits at four spaces and its list items at
# six, so a `patterns:` under `groups:` can never be read as a watched
# directory.
_ENTRY = re.compile(r"^  - package-ecosystem:\s*(?P<eco>\S+)\s*$")
_KEY = re.compile(r"^    (?P<key>[a-z-]+):\s*(?P<value>.*?)\s*$")
_LIST_ITEM = re.compile(r"^      - (?P<value>\S+)\s*$")
_IGNORE_ITEM = re.compile(r"^      - dependency-name:\s*(?P<value>\S+)\s*$")
_COMMENT = re.compile(r"^\s*#(?P<text>.*)$")


class ConfigUnreadable(RuntimeError):
    """`.github/dependabot.yml` is absent or parses to nothing usable."""


def parse_config(text: str) -> list[dict]:
    """[{ecosystem, line, directories: [(value, line)], ignores: [...]}].

    Each ignore carries the contiguous comment run immediately above it, which
    is where invariant 3 looks for the exit condition. A blank line or any
    config line between the comment and the `- dependency-name:` breaks that
    run, which is the right reading: a justification three keys away from the
    hold it justifies is not attached to it.
    """
    entries: list[dict] = []
    mode: str | None = None
    comments: list[str] = []
    for number, line in enumerate(text.split("\n"), 1):
        comment = _COMMENT.match(line)
        if comment:
            comments.append(comment.group("text").strip())
            continue
        if not line.strip():
            comments = []
            continue
        entry = _ENTRY.match(line)
        if entry:
            entries.append({"ecosystem": entry.group("eco"), "line": number,
                            "directories": [], "ignores": []})
            mode, comments = None, []
            continue
        if not entries:
            comments = []
            continue
        current = entries[-1]
        key = _KEY.match(line)
        if key:
            name, value = key.group("key"), key.group("value")
            if name == "directory" and value:
                current["directories"].append((value, number))
                mode = None
            elif name == "directories":
                mode = "directories"
            elif name == "ignore":
                mode = "ignore"
            else:
                mode = None
            comments = []
            continue
        if mode == "directories":
            item = _LIST_ITEM.match(line)
            if item:
                current["directories"].append((item.group("value"), number))
                comments = []
                continue
        if mode == "ignore":
            item = _IGNORE_ITEM.match(line)
            if item:
                current["ignores"].append(
                    {"name": item.group("value"), "line": number,
                     "comments": list(comments)})
                comments = []
                continue
        comments = []
    return entries


def read_config(path: pathlib.Path) -> list[dict]:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ConfigUnreadable(f"cannot read {path}: {exc}") from exc
    entries = parse_config(text)
    if not entries:
        raise ConfigUnreadable(
            f"{path} parsed to zero `package-ecosystem` entries -- a coverage "
            "check run against nothing passes vacuously, which is the failure "
            "this file exists to prevent")
    return entries


# --- resolving what an entry actually watches --------------------------------

def resolve(value: str, root: pathlib.Path) -> list[str]:
    """Repo-relative directories a `directory`/`directories` value names.

    Empty when it resolves to nothing on disk, which is invariant 2's failure.
    """
    rel = value.strip().lstrip("/")
    if not rel:
        return [""] if root.is_dir() else []
    if any(ch in rel for ch in "*?["):
        return sorted(
            p.relative_to(root).as_posix()
            for p in root.glob(rel) if p.is_dir()
        )
    return [rel] if (root / rel).is_dir() else []


def holds_manifest(reldir: str, ecosystem: str, by_ecosystem: dict[str, set[str]],
                   root: pathlib.Path) -> bool:
    """Is there anything in `reldir` for Dependabot to update?

    github-actions is the one ecosystem with no manifest in the tracked set --
    its input is `.github/workflows/`, and it is only ever declared at the root.
    """
    if ecosystem == "github-actions":
        return reldir == "" and (root / ".github" / "workflows").is_dir()
    return reldir in by_ecosystem.get(ecosystem, set())


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument(
        "--strict", action="store_true",
        help="also refuse to pass vacuously: no manifests discovered at all, "
             "or a deliberate exclusion that no longer excuses anything")
    args = parser.parse_args(argv)

    root = ROOT
    try:
        entries = read_config(CONFIG)
    except ConfigUnreadable as exc:
        print(f"check_dependency_risk: {exc}", file=sys.stderr)
        return 1

    # path -> ecosystem, in one pass, so nothing downstream has to re-ask a
    # question that can answer None.
    ecosystems: dict[str, str] = {}
    for path in tracked_files(root):
        found_eco = ecosystem_of(path)
        if found_eco:
            ecosystems[path] = found_eco
    manifests = sorted(ecosystems)
    by_ecosystem: dict[str, set[str]] = {}
    for path in manifests:
        by_ecosystem.setdefault(ecosystems[path], set()).add(directory_of(path))

    problems: list[str] = []

    # --- INVARIANT 2, first: an entry pointing at nothing cannot cover -------
    watched: dict[str, set[str]] = {}
    for entry in entries:
        ecosystem = entry["ecosystem"]
        for value, line in entry["directories"]:
            found = resolve(value, root)
            if not found:
                problems.append(
                    f"{CONFIG.name}:{line}: the {ecosystem} entry watches "
                    f"{value!r}, which matches NO directory in this "
                    "repository. Dependabot does not report that anywhere a "
                    "person reads -- the job fails at file fetching every run "
                    "(`Repo must contain a requirements.txt, uv.lock, ...`) "
                    "while any alert naming that path outlives the path")
                continue
            live = [d for d in found
                    if holds_manifest(d, ecosystem, by_ecosystem, root)]
            if not live:
                shown = ", ".join(found[:5])
                problems.append(
                    f"{CONFIG.name}:{line}: the {ecosystem} entry watches "
                    f"{value!r}, which resolves to {shown} -- and none of those "
                    f"holds a {ecosystem} manifest, so there is nothing there "
                    "for Dependabot to update")
            watched.setdefault(ecosystem, set()).update(found)

    # --- INVARIANT 1: every manifest is covered by some entry ----------------
    members = pnpm_members(root)
    used_exclusions: set[str] = set()
    for path in manifests:
        ecosystem = ecosystems[path]
        directory = directory_of(path)
        # A workspace member's dependencies live in the root lockfile, so the
        # root entry is what genuinely covers it. Resolved rather than
        # excluded, because it IS watched -- just not where it sits.
        if ecosystem == "npm" and directory in members:
            directory = ""
        if directory in watched.get(ecosystem, set()):
            continue
        hit = [pattern for pattern, _ in EXCLUDED if matches(pattern, path)]
        if hit:
            used_exclusions.update(hit)
            continue
        problems.append(
            f"{path} is a {ecosystem} manifest that NO entry in "
            f"{CONFIG.name} watches. Nothing scans it, and a dependency "
            "scanner that has stopped matching the tree reports clean on "
            "exactly the manifests nobody is watching. Add a `directories` "
            f"entry covering /{directory}, or record the path in EXCLUDED in "
            f"{pathlib.Path(__file__).name} with the reason it is deliberate")

    # --- INVARIANT 3: every hold declares how it ends ------------------------
    for entry in entries:
        for hold in entry["ignores"]:
            joined = " ".join(hold["comments"])
            if any(marker in joined for marker in EXIT_MARKERS):
                continue
            problems.append(
                f"{CONFIG.name}:{hold['line']}: the {entry['ecosystem']} hold "
                f"on {hold['name']!r} declares no exit condition. The comment "
                f"run directly above it must say {EXIT_MARKERS[0]!r} and name "
                "the upstream change that retires the hold, or "
                f"{EXIT_MARKERS[1]!r} if it is policy rather than delay. A "
                "hold justified by a temporary fact becomes a permanent "
                "decision on the strength of that fact unless something "
                "re-asks the question")

    if args.strict:
        if not manifests:
            problems.append(
                "no dependency manifests were discovered at all, so this "
                "check inspected nothing and would pass whatever the config "
                "said")
        stale = [p for p, _ in EXCLUDED if p not in used_exclusions]
        if stale:
            problems.append(
                "these deliberate exclusions excuse no unwatched manifest, so "
                "they are carrying nothing and would hide the next real "
                f"omission: {stale}")

    if problems:
        print("check_dependency_risk:\n  " + "\n\n  ".join(problems),
              file=sys.stderr)
        return 1

    tally: dict[str, int] = {}
    for path in manifests:
        tally[ecosystems[path]] = tally.get(ecosystems[path], 0) + 1
    counts = ", ".join(f"{n} {eco}" for eco, n in sorted(tally.items()))
    excused = sum(1 for path in manifests
                  if any(matches(pattern, path) for pattern, _ in EXCLUDED))
    print(f"check_dependency_risk: {len(manifests)} tracked manifests "
          f"({counts}) -- {len(manifests) - excused} watched by "
          f"{len(entries)} Dependabot entries, {excused} deliberately excluded "
          f"with a reason; every watched path resolves to a directory that "
          f"holds a manifest; all "
          f"{sum(len(e['ignores']) for e in entries)} ignore holds declare an "
          "exit condition")
    return 0


if __name__ == "__main__":
    sys.exit(main())
