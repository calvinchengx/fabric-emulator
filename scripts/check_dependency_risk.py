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

THE FIVE INVARIANTS. The first three ask whether Dependabot still matches
the tree; the last two ask whether anything SCANS what it finds there.

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

4. **EVERY ECOSYSTEM WITH TRACKED MANIFESTS IS REACHED BY A SCANNER JOB.**
   Invariants 1-3 ask whether a manifest is WATCHED; this asks whether
   anything SCANS it for advisories on every push, which is a different
   question with the same symptom when the answer is no. `SECURITY.md` carried
   the gap under *what does not run* -- **Go had `govulncheck` and Python and
   npm had Dependabot alerts alone** -- and Python is the largest surface
   here: nine `uv.lock` files, 856 locked packages, seven of them the examples
   people copy out. The `SCANNERS` table above maps each ecosystem to the job
   that scans it, with `docker` and `github-actions` recorded as deliberately
   on Dependabot alone and the reason stated, because an ecosystem nobody has
   decided about is the state those seven lockfiles were in.

   **AND THOSE JOBS MUST DISCOVER THEIR INPUTS, NOT LIST THEM.** A `run:`
   command in a scan job may not name a lockfile path. The seven unwatched
   example lockfiles are what a hand-maintained list looks like after the tree
   moves, so the discovery design is enforced here rather than left as an
   intention a future edit could quietly reverse.

5. **EVERY ADVISORY HOLD DECLARES ITS EXIT CONDITION.** Invariant 3's rule,
   applied to `docs/advisory-holds.json` -- the third place this repository
   can decide to live with a known risk, after a Dependabot `ignore:` and a
   dismissed alert. `cryptography` is the worked example and the reason this
   matters: it was held below 50 because mlflow's `requires_dist` forbade it,
   and the hold's own comment predicted the cost -- *"cryptography is
   security-relevant and 49.x carries no open advisory today, but that could
   change while the ceiling holds."* It changed. `PYSEC-2026-3552` landed
   against 49.0.0 with its fix in exactly the 50.0.0 the ceiling forbade, and
   nothing was asking whether the prediction had come true until the scan in
   invariant 4 started looking. mlflow 3.16.1 has since raised the ceiling to
   `<51`, which is that hold's `LIFT THIS` condition met.

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
import json
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
CONFIG = ROOT / ".github" / "dependabot.yml"
WORKFLOW = ROOT / ".github" / "workflows" / "security.yml"
HOLDS = ROOT / "docs" / "advisory-holds.json"

# A hold is temporary or it is policy. Either is fine; an undeclared one is not.
EXIT_MARKERS = ("LIFT THIS", "PERMANENT HOLD")

# WHICH JOB IN security.yml ADVISORY-SCANS EACH ECOSYSTEM. Dependabot gives
# every ecosystem version currency plus GitHub's advisory graph; this table is
# about the scan that runs IN this repository, on every push, where a finding
# blocks a merge rather than waiting in a tab. Two entries scan nothing, and
# they say so with their reason -- the same rule EXCLUDED follows above, for
# the same reason: adding a scanner nothing watches recreates the problem this
# file exists to solve, and so does quietly having no scanner at all.
SCANNERS: tuple[tuple[str, str | None, str], ...] = (
    (
        "gomod", "vulnerabilities",
        "govulncheck ./... -- the only one of the three that is "
        "reachability-filtered, so a finding is a call path rather than a "
        "dependency-tree coincidence",
    ),
    (
        "uv", "python-advisories",
        "osv-scanner over every tracked uv.lock, version-level against the "
        "advisory graph. This repository's largest dependency surface: nine "
        "lockfiles, seven of them the examples people copy out of here",
    ),
    (
        "npm", "js-advisories",
        "osv-scanner over the root pnpm-lock.yaml, which pnpm-workspace.yaml "
        "makes cover portal and website too",
    ),
    (
        "docker", None,
        "NO ADVISORY SCAN, deliberately. The input is a base-image tag, not a "
        "resolved dependency set, so there is no locked version list to match "
        "against advisories -- scanning it would mean pulling and scanning "
        "image layers, which is a different tool and a different job budget. "
        "Dependabot's base-image bumps are the whole of the coverage here, "
        "and docs/63-security-footguns.md records that honestly",
    ),
    (
        "github-actions", None,
        "NO ADVISORY SCAN, deliberately, and no manifest either: the input is "
        ".github/workflows/ itself. Actions are pinned by SHA where it "
        "matters (see ci.yml's setup-uv) and there is no advisory database "
        "keyed on action versions to scan against. Dependabot alone",
    ),
)

# A scan job must not NAME a lockfile. Its inputs come from `git ls-files`, and
# a hand-maintained list is the precise failure this file exists to prevent --
# seven example uv.lock files watched by nothing, found only through a stale
# alert naming a path deleted months earlier. A literal path in the command
# would drift from the tree in exactly the same silence, so the discovery
# design is enforced rather than merely intended.
LOCKFILE_NAMES = ("uv.lock", "pnpm-lock.yaml", "package-lock.json")

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


# --- reading .github/workflows/security.yml ----------------------------------
#
# Indent-anchored like the dependabot reader above, and for the same reason: a
# job key sits at two spaces under `jobs:` and its steps deeper, so a `name:`
# inside a step can never be read as a job.


class WorkflowUnreadable(RuntimeError):
    """`security.yml` is absent or parses to zero jobs."""


def parse_jobs(text: str) -> dict[str, list[str]]:
    """{job key: its `run:` command lines}.

    Only `run:` lines, deliberately. The job's `name:` and its comment block
    are prose that legitimately mentions `uv.lock` -- this job is called
    "Python advisories (9 uv.lock files)" -- so scanning the whole body for a
    lockfile name would fail the file for describing itself accurately. What
    must not name a lockfile is the COMMAND, because that is the thing that
    would drift from the tree.
    """
    jobs: dict[str, list[str]] = {}
    current: str | None = None
    in_jobs = False
    for line in text.split("\n"):
        if re.match(r"^jobs:\s*$", line):
            in_jobs = True
            continue
        if not in_jobs:
            continue
        if line.strip() and not line.startswith((" ", "\t", "#")):
            break
        key = re.match(r"^  (?P<key>[A-Za-z0-9_-]+):\s*$", line)
        if key:
            current = key.group("key")
            jobs.setdefault(current, [])
            continue
        if current and re.match(r"^\s*#", line):
            continue
        run = re.match(r"^\s+-?\s*run:\s*(?P<cmd>.*)$", line)
        if run and current:
            jobs[current].append(run.group("cmd").strip())
    return jobs


def read_jobs(path: pathlib.Path) -> dict[str, list[str]]:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise WorkflowUnreadable(f"cannot read {path}: {exc}") from exc
    jobs = parse_jobs(text)
    if not jobs:
        raise WorkflowUnreadable(
            f"{path} parsed to zero jobs -- a check that cannot see the "
            "scanner jobs would report every ecosystem unscanned, or pass "
            "vacuously, depending on which way it read the silence")
    return jobs


def read_holds(path: pathlib.Path) -> list[dict]:
    """The advisory holds, or an empty list when the ledger is absent.

    Absent is legitimate and means "nothing is held": the ledger is created
    only when a triage finds an advisory that cannot be fixed, because an
    empty one would hide nothing and carry nothing.
    """
    if not path.is_file():
        return []
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise WorkflowUnreadable(f"cannot read {path}: {exc}") from exc
    holds = data.get("holds")
    if not isinstance(holds, list):
        raise WorkflowUnreadable(f"{path} declares no `holds` list")
    return holds


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

    # --- INVARIANT 4: every ecosystem with manifests has a scanner ----------
    #
    # Dependabot coverage (invariants 1-3) answers "is this manifest WATCHED".
    # This answers the different question SECURITY.md's "what does not run"
    # section was written to keep honest: is anything SCANNING it for
    # advisories on every push. Both gaps produce a green check.
    try:
        jobs = read_jobs(WORKFLOW)
    except WorkflowUnreadable as exc:
        problems.append(f"{exc}")
        jobs = {}

    scanners = {eco: (job, reason) for eco, job, reason in SCANNERS}
    for ecosystem in sorted(by_ecosystem):
        if ecosystem not in scanners:
            problems.append(
                f"{ecosystem} has {len(by_ecosystem[ecosystem])} tracked "
                f"manifest directory/ies and is not named in SCANNERS in "
                f"{pathlib.Path(__file__).name}. Either a job in "
                f"{WORKFLOW.name} advisory-scans it, or the reason nothing "
                "does is written down there -- an ecosystem nobody has "
                "decided about is the state seven example lockfiles were in")
            continue
        job, _ = scanners[ecosystem]
        if job is not None and jobs and job not in jobs:
            problems.append(
                f"{WORKFLOW.name} declares no job {job!r}, which SCANNERS in "
                f"{pathlib.Path(__file__).name} records as the advisory scan "
                f"for {ecosystem}. A renamed or deleted scan job leaves the "
                "ecosystem unscanned while this table still claims it is "
                "covered, which is the same green check as an unwatched "
                "manifest")

    # A scan job must DISCOVER its lockfiles, not list them.
    for _ecosystem, job, _ in SCANNERS:
        if job is None or job not in jobs:
            continue
        for command in jobs[job]:
            named = [n for n in LOCKFILE_NAMES if n in command]
            if named:
                problems.append(
                    f"{WORKFLOW.name}: job {job!r} names {named} in a `run:` "
                    f"command: {command!r}. A scan job's inputs must come "
                    "from `git ls-files` so a new lockfile is covered by "
                    "code already in the tree. A hand-maintained path list "
                    "drifts from the repository in silence -- which is how "
                    "seven example uv.lock files came to be watched by "
                    "nothing, found only through a stale alert naming a "
                    "directory deleted months earlier")

    # --- INVARIANT 5: every advisory hold declares how it ends --------------
    #
    # The rule invariant 3 enforces for dependabot.yml's `ignore:` entries and
    # check_dismissed_advisories.py enforces one file over, applied to the
    # third place this repository can decide to live with a known risk. A hold
    # justified by a temporary fact becomes permanent unless something
    # re-asks the question, and `cryptography` is the worked example: held
    # below 50 because mlflow forbade it, with the comment predicting that
    # 49.x might not stay advisory-free -- which is exactly what happened.
    try:
        holds = read_holds(HOLDS)
    except WorkflowUnreadable as exc:
        problems.append(f"{exc}")
        holds = []

    for index, hold in enumerate(holds):
        where = hold.get("id") or f"holds[{index}]"
        exit_text = hold.get("exit") or ""
        if not exit_text:
            problems.append(
                f"{HOLDS.name}: the advisory hold on {where} declares no "
                "`exit` at all. An accepted advisory with no exit condition "
                "is indistinguishable from one nobody looked at")
            continue
        if not any(marker in exit_text for marker in EXIT_MARKERS):
            problems.append(
                f"{HOLDS.name}: the advisory hold on {where} declares no exit "
                f"condition. Its `exit` must say {EXIT_MARKERS[0]!r} and name "
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
        # The same discipline for SCANNERS. A row for an ecosystem that has
        # left the tree reads as coverage of something, and github-actions is
        # the one legitimate exception -- its input is .github/workflows/, so
        # it has no manifest in the tracked set by construction.
        orphans = [eco for eco, _, _ in SCANNERS
                   if eco not in by_ecosystem and eco != "github-actions"]
        if orphans:
            problems.append(
                "these SCANNERS rows name an ecosystem with no tracked "
                "manifest, so they describe coverage of nothing and would "
                f"make the next real gap look decided: {orphans}")

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
    scanned = ", ".join(
        f"{eco} by {job}" for eco, job, _ in SCANNERS
        if job is not None and eco in by_ecosystem)
    unscanned = [eco for eco, job, _ in SCANNERS
                 if job is None and eco in by_ecosystem]
    print(f"check_dependency_risk: {len(manifests)} tracked manifests "
          f"({counts}) -- {len(manifests) - excused} watched by "
          f"{len(entries)} Dependabot entries, {excused} deliberately excluded "
          f"with a reason; every watched path resolves to a directory that "
          f"holds a manifest; all "
          f"{sum(len(e['ignores']) for e in entries)} ignore holds declare an "
          f"exit condition; advisory-scanned in {WORKFLOW.name}: {scanned} "
          f"(discovering lockfiles from git, naming none), {len(unscanned)} "
          f"ecosystem(s) deliberately on Dependabot alone with a reason "
          f"({', '.join(unscanned)}); all {len(holds)} advisory holds in "
          f"{HOLDS.name} declare an exit condition")
    return 0


if __name__ == "__main__":
    sys.exit(main())
