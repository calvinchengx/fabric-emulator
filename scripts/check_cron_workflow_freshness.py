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


def _triggers(doc) -> set:
    """The `on:` keys. PyYAML parses a bare `on:` as the boolean True."""
    on = doc.get(True, doc.get("on", {}))
    if isinstance(on, str):
        return {on}
    if isinstance(on, list):
        return set(on)
    return set(on or {})


def _last_changed(path: pathlib.Path) -> int:
    """Committer epoch seconds of the last commit touching this file.

    Epoch, NOT an ISO string: `%cI` carries a local offset (`+08:00`) while the
    API returns UTC `Z`, and comparing those two as text reported drift that did
    not exist and hid drift that did. This exact mistake was made while writing
    this script.
    """
    out = subprocess.run(
        ["git", "log", "-1", "--format=%ct", "--", str(path)],
        cwd=ROOT, capture_output=True, text=True, check=True).stdout.strip()
    return int(out) if out else 0


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
    # pyyaml is a DECLARED dependency, so a missing import means a broken
    # environment, not a laptop without extras. pyproject.toml records the same
    # trap being sprung once already ("a developer machine usually has pyyaml
    # lying around"), so this imports hard rather than skipping.
    import yaml

    in_ci = os.environ.get("GITHUB_ACTIONS") == "true"
    if not shutil.which("gh"):
        if in_ci:
            print("FAIL: the GitHub CLI is unavailable IN CI, so run history "
                  "cannot be read and this check would verify nothing.")
            return 1
        print("check_cron_workflow_freshness: SKIPPED — the GitHub CLI is not on PATH")
        return 0

    cron_only, stale, unknown = [], [], []
    for path in sorted(WORKFLOWS.glob("*.yml")):
        if path.name in EXEMPT:
            continue
        doc = yaml.safe_load(path.read_text(encoding="utf-8"))
        if not isinstance(doc, dict) or _triggers(doc) & PUSH_TRIGGERS:
            continue
        cron_only.append(path.stem)
        changed = _last_changed(path)
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
