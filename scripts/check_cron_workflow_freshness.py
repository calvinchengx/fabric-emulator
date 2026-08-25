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
workflow correct" — it is the cheaper, checkable one: **has anyone run THIS
VERSION of it?**

IT COMPARES CONTENT, NOT DATES, and the difference is not academic. The first
version asked "did a run happen after the file's last-changed timestamp", and
its own remediation told you to dispatch on the branch that changed the
workflow. Following that advice could not satisfy it: **merging re-dates the
file**, so a squash commit is always newer than the pre-merge run, and main
went red immediately after every such merge. The person who did exactly what
the message asked was the person who broke main — twice, in this repository,
before anyone noticed the shape of it.

A run proves a VERSION of the workflow, and a version is its bytes. So this
hashes the workflow as it stands and asks whether any recent run executed the
same bytes. A merge that moves a file without changing it now counts, because
what ran and what is committed are the same workflow.

WHAT IT DELIBERATELY DOES NOT DO. It does not fail on a workflow that has never
run at all when the repository has no runs to read (a fresh clone, a fork), and
it does not police whether the run PASSED — a red cron run is a normal finding
that the run itself reports. Freshness is the property no other check covers.

Needs the GitHub API for run times, so it SKIPS with a clear message when `gh`
is unavailable or unauthenticated rather than failing a laptop that has neither.
A check that cannot run is not a check that failed, and saying which is the
difference between a signal and noise.
"""
import base64
import datetime
import hashlib
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
UNREADABLE = object()

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


def _repo_slug() -> str:
    r = subprocess.run(["gh", "repo", "view", "--json", "nameWithOwner",
                        "--jq", ".nameWithOwner"],
                       cwd=ROOT, capture_output=True, text=True)
    return r.stdout.strip() if r.returncode == 0 else ""


def _digest(text: str) -> str:
    """A workflow version's identity.

    Line endings are normalised first: a Windows checkout holds CRLF where git
    stores LF, and hashing raw bytes would report every workflow stale on one
    of the three CI platforms.
    """
    return hashlib.sha256(text.replace("\r\n", "\n").encode("utf-8")).hexdigest()


_AT_SHA: dict = {}


def _digest_at(sha: str, rel: str, slug: str):
    """The workflow's digest as of `sha`, or None when it cannot be read.

    Local git first — in a full clone the object is usually right there, and it
    costs no API call. The API is the fallback for a shallow checkout, and for
    a run whose branch has since been deleted: GitHub keeps the commit even
    when the ref is gone, which is exactly the case after a squash-merge.
    """
    if (sha, rel) in _AT_SHA:
        return _AT_SHA[(sha, rel)]
    text = None
    local = subprocess.run(["git", "show", f"{sha}:{rel}"],
                           cwd=ROOT, capture_output=True, text=True)
    if local.returncode == 0:
        text = local.stdout
    elif slug:
        api = subprocess.run(
            ["gh", "api", f"repos/{slug}/contents/{rel}?ref={sha}", "--jq", ".content"],
            cwd=ROOT, capture_output=True, text=True)
        if api.returncode == 0 and api.stdout.strip():
            try:
                text = base64.b64decode(api.stdout.strip()).decode("utf-8")
            except (ValueError, UnicodeDecodeError):
                text = None
    result = _digest(text) if text is not None else None
    _AT_SHA[(sha, rel)] = result
    return result


def _run_of_this_version(stem: str, rel: str, want: str, slug: str):
    """(epoch, conclusion) of a run that executed the CURRENT bytes, or None.

    Returns the sentinel `UNREADABLE` when runs exist but none of their
    versions could be resolved — that is not evidence of staleness, and
    reporting it as such would fail a job that merely lost `contents: read`.
    """
    r = subprocess.run(
        ["gh", "run", "list", "--workflow", f"{stem}.yml", "--limit", "20",
         "--json", "createdAt,conclusion,headSha"],
        cwd=ROOT, capture_output=True, text=True)
    if r.returncode != 0 or not r.stdout.strip():
        # Say WHY. Swallowing this is what made the first CI failure a guessing
        # game: "unreadable" is a symptom, and the stderr underneath it is the
        # diagnosis.
        why = (r.stderr or "").strip().replace("\n", " ")[:200]
        if why:
            print(f"    {stem}: gh run list failed — {why}")
        elif not r.stdout.strip():
            print(f"    {stem}: gh run list returned nothing (no runs?)")
        return None
    runs = json.loads(r.stdout)
    if not runs:
        return None
    resolved = 0
    for run in runs:
        got = _digest_at(run.get("headSha", ""), rel, slug)
        if got is None:
            continue
        resolved += 1
        if got == want:
            created = run["createdAt"].replace("Z", "+00:00")
            return (int(datetime.datetime.fromisoformat(created).timestamp()),
                    run.get("conclusion"))
    if resolved == 0:
        print(f"    {stem}: no run's workflow version could be read")
        return UNREADABLE
    # Runs exist and none of them ran these bytes: genuinely stale. The newest
    # is reported, because "what DID run last" is what a reader needs next.
    newest = runs[0]["createdAt"].replace("Z", "+00:00")
    return (int(datetime.datetime.fromisoformat(newest).timestamp()),
            runs[0].get("conclusion"), "stale")


def main() -> int:
    # STRICT is opt-in, and granted only where the API is reachable. `make
    # check` runs this in jobs that hold no GH_TOKEN — they are not lying about
    # freshness, they simply cannot see it, and failing them would make the
    # check noise. The one job that CAN see it sets FRESHNESS_STRICT=1, so a
    # permissions regression there still fails loudly instead of skipping.
    strict = os.environ.get("FRESHNESS_STRICT") == "1"
    if not shutil.which("gh"):
        if strict:
            print("FAIL: FRESHNESS_STRICT is set but the GitHub CLI is not on "
                  "PATH, so this check would verify nothing.")
            return 1
        print("check_cron_workflow_freshness: SKIPPED — the GitHub CLI is not on PATH")
        return 0

    shallow = _is_shallow()
    slug = _repo_slug() if shallow else ""
    if shallow and not slug:
        msg = ("shallow clone and the repository slug is unreadable, so the "
               "version a run executed cannot be determined")
        if strict:
            print(f"FAIL: {msg}.")
            return 1
        print(f"check_cron_workflow_freshness: SKIPPED — {msg}")
        return 0
    if shallow:
        print(f"shallow clone — workflow versions resolved from {slug} via the API")

    cron_only, stale, unknown = [], [], []
    for path in sorted(WORKFLOWS.glob("*.yml")):
        if path.name in EXEMPT:
            continue
        if _triggers(path.read_text(encoding="utf-8")) & PUSH_TRIGGERS:
            continue
        cron_only.append(path.stem)
        rel = path.relative_to(ROOT).as_posix()
        want = _digest(path.read_text(encoding="utf-8"))
        # `slug` is only resolved for a shallow clone above, but the API
        # fallback needs it whenever a run's commit is not in the local object
        # store — which is the ordinary case for a deleted branch.
        run = _run_of_this_version(path.stem, rel, want, slug or _repo_slug())
        if run is None or run is UNREADABLE:
            unknown.append(path.stem)
            continue
        if len(run) == 3:
            ran, conclusion, _ = run
            stale.append((path.stem, ran, conclusion))

    if not cron_only:
        print("FAIL: no cron-only workflows found — the trigger parse is broken,")
        print("      and a check that inspects nothing reports success forever.")
        return 1

    print(f"cron/dispatch-only workflows: {len(cron_only)} — {', '.join(cron_only)}")
    if unknown:
        print(f"  no run history readable: {', '.join(unknown)}")
        if strict:
            # Silence here is the failure mode: unreadable history means every
            # workflow looks fresh. Most likely cause is the job losing
            # `actions: read`.
            print("\nFAIL: the data above could not be read in CI, so every workflow")
            print("      would be reported fresh regardless. The per-workflow lines")
            print("      say which call failed — a last-changed lookup needs")
            print("      `contents: read`, run history needs `actions: read`.")
            return 1

    if stale:
        print("\nFAIL: this version has never run, so the change is unverified.")
        print("These never run on push, so nothing else will catch it:\n")
        for stem, ran, conclusion in stale:
            print(f"  {stem}")
            print(f"      no run has executed the committed workflow; the most "
                  f"recent run was {_utc(ran)} ({conclusion}) of a different version")
            print(f"      gh workflow run {stem}.yml --ref <a branch holding these bytes>")
        return 1

    print("every cron-only workflow has been run in its committed version")
    return 0


if __name__ == "__main__":
    sys.exit(main())
