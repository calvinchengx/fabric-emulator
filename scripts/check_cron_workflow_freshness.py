#!/usr/bin/env python3
"""Every cron/dispatch-only workflow has RUN since the last commit that changed it.

WHY THIS EXISTS. A workflow with `push:` or `pull_request:` triggers is proved
by the next commit. A workflow that only runs on `schedule:` or
`workflow_dispatch:` is not: a change to it is unverified until the cron fires,
which can be a week, and nothing between the merge and that firing says so.

That is not hypothetical. Three cron-only fixes were merged in one week and
none had executed:

  * `check_example_portability` and two siblings — enforced only by `make check`
    (a different gap, fixed separately; the shape is the same).
  * `real-fabric.yml`'s restructure — sound, once dispatched.
  * `e2e/spark-jvm`'s sc-facade oracle — **BROKEN**. It computed a HOST path for
    a module the container never mounts, so it died on ModuleNotFoundError and
    evaluated nothing. The PR claiming "verified against real Spark" was false
    from the moment it merged, and the weekly cron would have failed the same
    way indefinitely. Once repaired it immediately found a second defect.

Nothing in the diffs separated the sound changes from the broken one. Review
cannot see it; only execution can. So the question this asks is not "is the
workflow correct" — it is the cheaper, checkable one: **has anyone run it since
it last changed?**

WHAT IT DELIBERATELY DOES NOT DO. It does not fail on a workflow that has never
run at all when the repository has no runs to read (a fresh clone, a fork), and
it does not police whether the run PASSED — a red cron run is a normal finding
that the run itself reports. Freshness is the property no other check covers.

Needs the GitHub API for run times, so it SKIPS with a clear message when `gh`
is unavailable or unauthenticated rather than failing a laptop that has neither.
A check that cannot run is not a check that failed, and saying which is the
difference between a signal and noise.
"""
import datetime
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
WORKFLOWS = ROOT / ".github" / "workflows"

# Workflows exempt from freshness, WITH A REASON so an exemption is a decision
# someone wrote down rather than an omission nobody saw.
EXEMPT = {
    "release.yml": "fires on a tag; its freshness is the release itself",
}

PUSH_TRIGGERS = {"push", "pull_request", "pull_request_target", "merge_group"}


def _utc(ts: int) -> str:
    return datetime.datetime.fromtimestamp(ts, datetime.UTC).strftime("%Y-%m-%d %H:%MZ")


# STDLIB ONLY, like check_workflow_concurrency.py beside it. The first version
# used pyyaml — which is declared in the `test` group and absent from the jobs
# that run this, so it died on ModuleNotFoundError in three of them. The trap is
# written down in pyproject.toml ("a developer machine usually has pyyaml lying
# around and CI does not"), and a stale local .venv is exactly why it passed
# here first. A checker that needs a dependency its own job lacks is a checker
# that does not run.
_ON_BLOCK = re.compile(r"^on:\s*$(?P<body>(?:\n(?:[ \t]+.*|\s*))*)", re.M)
_ON_INLINE = re.compile(r"^on:[ \t]*(?P<value>\S.*)$", re.M)
_TOP_KEY = re.compile(r"^  ([A-Za-z_][A-Za-z0-9_-]*):", re.M)


def _triggers(text: str) -> set:
    """The `on:` keys of a workflow, read as text."""
    inline = _ON_INLINE.search(text)
    if inline:
        raw = inline.group("value").strip()
        if raw.startswith("["):
            return {t.strip().strip("'\"") for t in raw.strip("[]").split(",") if t.strip()}
        return {raw}
    block = _ON_BLOCK.search(text)
    if not block:
        return set()
    # No manual stop needed: the body pattern only accepts indented or blank
    # lines, so an unindented key (`jobs:`, `permissions:`) ends the match by
    # construction. A defensive loop here was dead — mutating it away changed
    # nothing, which is how it was found.
    return set(_TOP_KEY.findall(block.group("body")))


def _is_shallow() -> bool:
    out = subprocess.run(["git", "rev-parse", "--is-shallow-repository"],
                         cwd=ROOT, capture_output=True, text=True).stdout.strip()
    return out == "true"


def _last_changed_git(path: pathlib.Path) -> int:
    """Committer epoch seconds of the last commit touching this file.

    Epoch, NOT an ISO string: `%cI` carries a local offset (`+08:00`) while the
    API returns UTC `Z`, and comparing those two as text reported drift that did
    not exist and hid drift that did. That mistake was made while writing this.
    """
    out = subprocess.run(
        ["git", "log", "-1", "--format=%ct", "--", str(path)],
        cwd=ROOT, capture_output=True, text=True, check=True).stdout.strip()
    return int(out) if out else 0


def _head_sha() -> str:
    """The commit under test. For a PR, GITHUB_SHA is the merge commit, whose
    first parent history includes the branch — either resolves the PR's own
    change, which the default branch would not."""
    env = os.environ.get("GITHUB_SHA")
    if env:
        return env
    r = subprocess.run(["git", "rev-parse", "HEAD"], cwd=ROOT,
                       capture_output=True, text=True)
    return r.stdout.strip() if r.returncode == 0 else ""


def _last_changed_api(repo: str, rel: str, sha: str) -> int:
    """Same answer from the forge, for a SHALLOW clone.

    `actions/checkout` fetches depth 1, so `git log -- <path>` has no history to
    search and returns the TIP commit's date for every file. Every workflow then
    looks changed-just-now and the check fails all of them — which is exactly
    what it did on its first CI run. A shallow clone does not report an error
    here; it reports a plausible wrong number, so the fallback is chosen by
    detecting the clone, not by catching a failure.
    """
    # `sha` IS LOAD-BEARING. Without it the API answers for the DEFAULT BRANCH,
    # so a PR that changes a cron workflow — the one case this check exists for —
    # reads main's history and is never flagged. Verified against a real shallow
    # clone: the default-branch query reported "fresh" for a workflow the branch
    # had just modified.
    args = ["gh", "api", f"repos/{repo}/commits", "-X", "GET",
            "-f", f"path={rel}", "-f", "per_page=1",
            "--jq", ".[0].commit.committer.date"]
    if sha:
        args[6:6] = ["-f", f"sha={sha}"]
    r = subprocess.run(args, cwd=ROOT, capture_output=True, text=True)
    if r.returncode != 0 or not r.stdout.strip():
        return 0
    when = r.stdout.strip().replace("Z", "+00:00")
    return int(datetime.datetime.fromisoformat(when).timestamp())


def _repo_slug() -> str:
    r = subprocess.run(["gh", "repo", "view", "--json", "nameWithOwner",
                        "--jq", ".nameWithOwner"],
                       cwd=ROOT, capture_output=True, text=True)
    return r.stdout.strip() if r.returncode == 0 else ""


def _last_run(stem: str):
    """(epoch, conclusion) of the newest run, or None when unknown."""
    r = subprocess.run(
        ["gh", "run", "list", "--workflow", f"{stem}.yml", "--limit", "1",
         "--json", "createdAt,conclusion"],
        cwd=ROOT, capture_output=True, text=True)
    if r.returncode != 0 or not r.stdout.strip():
        return None
    runs = json.loads(r.stdout)
    if not runs:
        return None
    created = runs[0]["createdAt"].replace("Z", "+00:00")
    return int(datetime.datetime.fromisoformat(created).timestamp()), runs[0].get("conclusion")


def main() -> int:
    in_ci = os.environ.get("GITHUB_ACTIONS") == "true"
    if not shutil.which("gh"):
        if in_ci:
            print("FAIL: the GitHub CLI is unavailable IN CI, so run history "
                  "cannot be read and this check would verify nothing.")
            return 1
        print("check_cron_workflow_freshness: SKIPPED — the GitHub CLI is not on PATH")
        return 0

    shallow = _is_shallow()
    slug = _repo_slug() if shallow else ""
    if shallow and not slug:
        print("FAIL: shallow clone and the repository slug is unreadable, so the")
        print("      last-changed date cannot be determined for any workflow.")
        return 1
    head = _head_sha() if shallow else ""
    if shallow:
        print(f"shallow clone — last-changed dates from {slug}@{head[:7] or '?'} via the API")

    cron_only, stale, unknown = [], [], []
    for path in sorted(WORKFLOWS.glob("*.yml")):
        if path.name in EXEMPT:
            continue
        if _triggers(path.read_text(encoding="utf-8")) & PUSH_TRIGGERS:
            continue
        cron_only.append(path.stem)
        rel = path.relative_to(ROOT).as_posix()
        changed = _last_changed_api(slug, rel, head) if shallow else _last_changed_git(path)
        if not changed:
            unknown.append(path.stem)
            continue
        run = _last_run(path.stem)
        if run is None:
            unknown.append(path.stem)
            continue
        ran, conclusion = run
        if ran < changed:
            stale.append((path.stem, changed, ran, conclusion))

    if not cron_only:
        print("FAIL: no cron-only workflows found — the trigger parse is broken,")
        print("      and a check that inspects nothing reports success forever.")
        return 1

    print(f"cron/dispatch-only workflows: {len(cron_only)} — {', '.join(cron_only)}")
    if unknown:
        print(f"  no run history readable: {', '.join(unknown)}")
        if in_ci:
            # Silence here is the failure mode: unreadable history means every
            # workflow looks fresh. Most likely cause is the job losing
            # `actions: read`.
            print("\nFAIL: run history is unreadable in CI, so every workflow above")
            print("      would be reported fresh regardless. Check that this job")
            print("      still grants `actions: read`.")
            return 1

    if stale:
        print("\nFAIL: changed since they last ran, so the change is unverified.")
        print("These never run on push, so nothing else will catch it:\n")
        for stem, changed, ran, conclusion in stale:
            print(f"  {stem}")
            print(f"      changed {_utc(changed)}   last run {_utc(ran)} ({conclusion})")
            print(f"      gh workflow run {stem}.yml --ref <the branch that changed it>")
        return 1

    print("every cron-only workflow has run since its last change")
    return 0


if __name__ == "__main__":
    sys.exit(main())
