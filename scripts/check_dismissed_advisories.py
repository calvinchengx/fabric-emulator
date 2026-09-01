#!/usr/bin/env python3
"""A dismissal with no fix available is a promise to come back. Keep it.

WHY THIS EXISTS. Dependabot alert 56 (`GHSA-h7x2-h6g9-p789`, high: MLflow's AI
Gateway permits SSRF through an unvalidated `api_base`) was dismissed as
*tolerable risk* because there was nothing to upgrade to -- the vulnerable
range is `>= 3.13.0, <= 3.15.2` and 3.15.2 is the newest release. The exposure
is bounded by reachability, not removed.

**A DISMISSED ALERT NEVER COMES BACK.** GitHub will not re-raise it when
upstream ships a fix, and the justification written at dismissal time stays
true only until it isn't. So the dismissal quietly becomes a permanent
decision, made once on the strength of a fact that was temporary.

This asks the one question that would change the answer: has a patched version
appeared? GitHub's advisory API answers it, and the check FAILS when the answer
is yes -- because at that point the honest state is an open alert and a version
bump, not a dismissal.

WHERE IT RUNS, and why not in `make check`. It needs the network, and the
guards in `scripts/` are offline invariants run under `$(PY)`. It lives in
`security.yml` instead, which already runs on push, pull_request, a weekly
cron AND dispatch -- so it is exercised on every change as well as on a
schedule, and never becomes a cron-only workflow nobody has run. That file's
own header states the rule it is joining: *a scanner nobody runs is a scanner
that finds nothing.*

Usage:
    check_dismissed_advisories.py     exit non-zero if a dismissal has expired
"""
import json
import os
import pathlib
import sys
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
TRACKED = ROOT / ".github" / "dismissed-advisories.json"
API = "https://api.github.com/advisories/"


def fetch(ghsa: str) -> dict:
    """The advisory as GitHub currently states it.

    A token is used when one is present -- CI has `GITHUB_TOKEN` and the
    unauthenticated limit is 60/hour per IP -- but the endpoint is public, so
    this works on a laptop with no credentials.
    """
    req = urllib.request.Request(
        API + ghsa,
        headers={"Accept": "application/vnd.github+json",
                 "User-Agent": "fabric-emulator-dismissed-advisories"})
    token = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read().decode("utf-8"))


def review(entry: dict, advisory: dict) -> list[str]:
    """What, if anything, has changed since this was dismissed."""
    out = []
    name, eco = entry["package"], entry["ecosystem"]
    matches = [
        v for v in advisory.get("vulnerabilities") or []
        if (v.get("package") or {}).get("name") == name
        and (v.get("package") or {}).get("ecosystem") == eco
    ]
    if not matches:
        out.append(
            f"{entry['ghsa']}: no longer lists {eco} {name}. The advisory was "
            "withdrawn, re-scoped, or the package renamed — either way the "
            "dismissal rests on a statement that has changed")
        return out
    for v in matches:
        patched = (v.get("first_patched_version") or {})
        ident = patched.get("identifier") if isinstance(patched, dict) else patched
        if ident:
            out.append(
                f"{entry['ghsa']} ({eco} {name}): FIXED UPSTREAM in {ident}. "
                f"Alert {entry['alert']} was dismissed as {entry['reason']} "
                f"on {entry['dismissed']} because there was nothing to upgrade "
                f"to. There is now. Re-open it and take the fix — the "
                "dismissal's reasoning has expired")
    return out


def main(fetcher=fetch) -> int:
    tracked = json.loads(TRACKED.read_text(encoding="utf-8"))["dismissed"]
    if not tracked:
        print("check_dismissed_advisories: nothing dismissed")
        return 0
    problems, checked = [], 0
    for entry in tracked:
        try:
            advisory = fetcher(entry["ghsa"])
        except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as exc:
            problems.append(
                f"{entry['ghsa']}: could not be read ({exc}). The dismissal is "
                "unreviewed rather than confirmed still-valid")
            continue
        checked += 1
        problems.extend(review(entry, advisory))
    if problems:
        print("check_dismissed_advisories:\n  " + "\n  ".join(problems),
              file=sys.stderr)
        return 1
    for entry in tracked:
        print(f"  {entry['ghsa']} ({entry['package']}): still no fix upstream — "
              f"dismissal of alert {entry['alert']} holds")
    print(f"check_dismissed_advisories: {checked} dismissal(s) re-checked "
          "against the advisory API")
    return 0


if __name__ == "__main__":
    sys.exit(main())
