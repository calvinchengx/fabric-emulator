#!/usr/bin/env python3
"""Python and JavaScript dependencies get an advisory scan, not just alerts.

WHY THIS EXISTS. `SECURITY.md` listed this under *what does not run*, as an
honest known gap rather than a refusal: **Go had `govulncheck` and the other
two ecosystems had Dependabot alerts alone.** Python is this repository's
largest dependency surface -- nine tracked `uv.lock` files holding 856 locked
packages, seven of them the example lockfiles people copy out of here to start
with -- and nothing scanned any of them against an advisory database. A grep
for `pip-audit`, `osv-scanner`, `pnpm audit` and `trivy` across the workflows,
the Makefile and `pyproject.toml` returned nothing at all.

THE FIRST RUN FOUND SEVEN ADVISORIES over 1592 locked packages, which is the
whole argument for the file. Five were fixed rather than recorded (see
`docs/advisory-holds.json`), and one of those five is the reason to care:
`cryptography` 49.0.0 carried a CVSS 8.2 whose fixed version was exactly the
50.0.0 that `.github/dependabot.yml` was holding the package below. That
comment had written the risk down in advance -- *"49.x carries no open
advisory today, but that could change while the ceiling holds"* -- and nothing
was asking whether it had.

INPUTS ARE DISCOVERED, NEVER LISTED. `git ls-files` decides what gets scanned.
A hand-maintained path list is the exact failure `check_dependency_risk.py`
exists to prevent, and the seven unwatched example lockfiles are what it looks
like: a list drifts from the tree silently and the scan goes on reporting
green over the manifests it has stopped covering. Discovery means a new
example lockfile is scanned by code that is already here.

EVERY LOCKFILE IS SCANNED EVEN AFTER ONE FAILS, and the per-lockfile package
tally is printed on success. One bad lockfile must not mask the other eight,
and a pass has to be distinguishable from a no-op -- a scanner that silently
found no inputs reports exactly the same green as one that scanned everything.

WHAT IT IS NOT. This is a VERSION-LEVEL scan against the advisory graph. It is
not reachability-filtered the way `govulncheck` is, so a finding here means
"the locked version is affected", not "this code calls the affected symbol".
That residual gap is restated in `SECURITY.md` rather than papered over.

WHERE IT RUNS, and why not in `make check`. It needs the network and a
third-party binary, and the guards in `scripts/` are offline invariants run
under `$(PY)`. It lives in `security.yml` instead, which runs on push,
pull_request, a weekly cron AND dispatch -- so it is exercised on every change
as well as on a schedule, and never becomes a cron-only workflow nobody has
run. That file's own header states the rule it is joining: *a scanner nobody
runs is a scanner that finds nothing.* The offline half of this -- that every
ecosystem with lockfiles HAS a scanner job, and that every hold declares an
exit condition -- is `check_dependency_risk.py`'s fourth and fifth invariants,
because those are decidable from the tree and belong where `make check` runs.

Usage:
    check_advisories.py --ecosystem uv    scan every tracked uv.lock
    check_advisories.py --ecosystem npm   scan the root pnpm-lock.yaml
"""
from __future__ import annotations

import argparse
import json
import pathlib
import shutil
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
LEDGER = ROOT / "docs" / "advisory-holds.json"

# A hold is temporary or it is policy. Either is fine; an undeclared one is
# not. Same two markers check_dependency_risk.py enforces over dependabot.yml.
EXIT_MARKERS = ("LIFT THIS", "PERMANENT HOLD")

# One tool for both ecosystems, chosen over pip-audit + `pnpm audit` for a
# specific reason: `uv export` cannot produce a single requirements set for
# this repository at all. The root project declares CONFLICTING dependency
# groups (`dbt-fabric` vs `dbt-fabricspark`), so `uv export --all-groups`
# exits with `Groups ... are incompatible with the conflicts` and any smaller
# selection would scan a subset of the lockfile while reporting on the whole
# of it. osv-scanner reads `uv.lock` and `pnpm-lock.yaml` natively, so it
# scans what is actually PINNED -- the union across conflicting groups
# included -- which is the set a reader of the lockfile would expect.
SCANNER = "osv-scanner"

# Pinned, like gitleaks in the same workflow: an advisory scanner that
# silently changes its lockfile parsing between runs makes a green check mean
# something different from week to week.
SCANNER_VERSION = "2.2.3"

# What `git ls-files` pattern feeds each ecosystem, and why that is the whole
# of it. Keyed by the same ecosystem names Dependabot uses, so
# check_dependency_risk.py's fourth invariant can line these up against
# `.github/dependabot.yml` without a translation table.
ECOSYSTEMS: dict[str, tuple[str, ...]] = {
    # Nine files: the root, e2e/fabric-cli, and the seven examples. Globbed
    # rather than listed -- see the module docstring.
    "uv": ("uv.lock", "*/uv.lock"),
    # One file. `pnpm-workspace.yaml` folds `portal` and `website` into the
    # root lockfile, so this single path genuinely covers all three packages;
    # that is the same fact check_dependency_risk.py's `pnpm_members` reads
    # when it resolves a workspace member to the root entry.
    "npm": ("pnpm-lock.yaml",),
}


class LedgerUnreadable(RuntimeError):
    """`docs/advisory-holds.json` is absent or does not parse."""


def tracked(patterns: tuple[str, ...], root: pathlib.Path) -> list[str]:
    """Repo-relative lockfiles git knows about, matching any pattern.

    `git ls-files` rather than a filesystem walk, which keeps `node_modules/`,
    `.venv/`, `third_party/` and `.claude/worktrees/` out without a
    hand-maintained deny list -- and keeps an UNTRACKED lockfile out too,
    since CI only ever has what was committed.
    """
    out = subprocess.run(
        ["git", "-C", str(root), "ls-files", *patterns],
        check=True, capture_output=True, text=True,
    ).stdout
    return sorted(line for line in out.split("\n") if line)


def read_ledger(path: pathlib.Path) -> list[dict]:
    """The accepted advisories, or an empty list when the file is absent.

    Absent is legitimate and means "nothing is held": the ledger is only
    created when a triage actually finds something unfixable, because an empty
    one would hide nothing and carry nothing. Present-but-broken is not
    legitimate, and raises -- a ledger that fails to parse would otherwise
    read as "no holds" and turn every accepted finding into a build failure
    with a misleading message.
    """
    if not path.is_file():
        return []
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise LedgerUnreadable(f"cannot read {path}: {exc}") from exc
    holds = data.get("holds")
    if not isinstance(holds, list):
        raise LedgerUnreadable(f"{path} declares no `holds` list")
    return holds


def hold_problems(holds: list[dict]) -> list[str]:
    """Entries that are missing a required field or an exit condition.

    Checked here as well as in `check_dependency_risk.py` so the gating job
    does not depend on `make check` having run: a hold with no exit condition
    must not be able to suppress a finding on the strength of a field nobody
    validated.
    """
    problems = []
    for index, hold in enumerate(holds):
        where = hold.get("id") or f"holds[{index}]"
        for field in ("id", "ecosystem", "package", "lockfile", "why", "exit"):
            if not hold.get(field):
                problems.append(
                    f"{LEDGER.name}: the hold on {where} declares no "
                    f"{field!r}. An accepted advisory with no reason is "
                    "indistinguishable from one nobody looked at")
        exit_text = hold.get("exit") or ""
        if exit_text and not any(m in exit_text for m in EXIT_MARKERS):
            problems.append(
                f"{LEDGER.name}: the hold on {where} declares no exit "
                f"condition. Its `exit` must say {EXIT_MARKERS[0]!r} and name "
                "the upstream change that retires the hold, or "
                f"{EXIT_MARKERS[1]!r} if it is policy rather than delay. A "
                "hold justified by a temporary fact becomes a permanent "
                "decision on the strength of that fact unless something "
                "re-asks the question")
    return problems


def scan(lockfile: str, root: pathlib.Path) -> tuple[int, list[dict]]:
    """(packages scanned, findings) for one lockfile.

    osv-scanner exits 1 when it finds something and 0 when it does not, so the
    exit status is not an error signal here -- only a failure to produce JSON
    is. Returning the package count alongside the findings is what lets the
    caller prove the scan had an input: `0 packages` from a file that exists
    means the parse silently did nothing, which looks identical to clean.
    """
    proc = subprocess.run(
        # `--all-packages` is what makes the package count mean anything. By
        # default the JSON report lists only packages that HAVE a finding, so
        # a clean lockfile and a lockfile the parser silently ignored both come
        # back as zero -- the two cases this function exists to tell apart.
        # Caught by the zero-package guard below on its first real run against
        # nine clean lockfiles, which is the argument for having that guard.
        [SCANNER, "scan", "source", "--lockfile", lockfile,
         "--format", "json", "--all-packages"],
        cwd=str(root), capture_output=True, text=True, check=False,
    )
    try:
        report = json.loads(proc.stdout)
    except json.JSONDecodeError as exc:
        raise RuntimeError(
            f"{SCANNER} produced no JSON for {lockfile} (exit "
            f"{proc.returncode}): {proc.stderr.strip()[:400]}") from exc

    packages, findings = 0, []
    for result in report.get("results", []):
        for package in result.get("packages", []):
            packages += 1
            info = package.get("package", {})
            for vuln in package.get("vulnerabilities", []) or []:
                findings.append({
                    "lockfile": lockfile,
                    "package": info.get("name", "?"),
                    "version": info.get("version", "?"),
                    "id": vuln.get("id", "?"),
                    # Every alias, because the ledger is keyed on one id and
                    # OSV reports the same advisory under PYSEC/GHSA/CVE
                    # names interchangeably. Matching on the primary id alone
                    # would let a hold stop working when OSV reshuffles which
                    # name is primary -- silently, and in the direction that
                    # turns an accepted finding into a red build.
                    "aliases": set(vuln.get("aliases", []) or []),
                    "summary": (vuln.get("summary") or "").strip(),
                })
    return packages, findings


def held_by(finding: dict, holds: list[dict]) -> dict | None:
    """The ledger entry covering this finding, if any.

    Matched on id-or-alias AND lockfile, so a hold reasoned about one lockfile
    cannot silently excuse the same advisory somewhere its argument was never
    made -- the build-time-only reasoning for a website dependency says
    nothing about the same package arriving in a shipped image.
    """
    names = {finding["id"], *finding["aliases"]}
    for hold in holds:
        if hold.get("id") in names and hold.get("lockfile") == finding["lockfile"]:
            return hold
    return None


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--ecosystem", required=True, choices=sorted(ECOSYSTEMS),
                        help="which tracked lockfiles to scan")
    args = parser.parse_args(argv)

    root, ecosystem = ROOT, args.ecosystem

    if shutil.which(SCANNER) is None:
        # Loud, and a FAILURE rather than a skip. check_dismissed_advisories
        # skips when `gh` is missing because a laptop legitimately has no
        # GitHub CLI; this runs only in CI, where a missing scanner means the
        # install step broke and the job would otherwise report green having
        # scanned nothing.
        print(f"check_advisories: {SCANNER} is not on PATH, so nothing was "
              f"scanned. This job installs v{SCANNER_VERSION} before running; "
              "a green check here without it would mean the install step "
              "failed silently.", file=sys.stderr)
        return 1

    try:
        holds = read_ledger(LEDGER)
    except LedgerUnreadable as exc:
        print(f"check_advisories: {exc}", file=sys.stderr)
        return 1

    problems = hold_problems(holds)

    lockfiles = tracked(ECOSYSTEMS[ecosystem], root)
    if not lockfiles:
        print(f"check_advisories: no tracked {ecosystem} lockfiles matched "
              f"{list(ECOSYSTEMS[ecosystem])}, so this job inspected nothing "
              "and would pass whatever the dependencies said. Either the "
              "patterns have drifted from the tree or the ecosystem is gone; "
              "both need a human.", file=sys.stderr)
        return 1

    # Accumulated, never short-circuited: one unparseable lockfile must not
    # hide the findings in the other eight.
    tally: list[tuple[str, int]] = []
    findings: list[dict] = []
    for lockfile in lockfiles:
        try:
            packages, found = scan(lockfile, root)
        except RuntimeError as exc:
            problems.append(str(exc))
            continue
        if packages == 0:
            problems.append(
                f"{lockfile}: {SCANNER} parsed it and reported ZERO packages. "
                "A lockfile that scans to nothing reports clean for the same "
                "reason an unwatched one does, so this is a failure rather "
                "than a pass")
        tally.append((lockfile, packages))
        findings.extend(found)

    unheld = [f for f in findings if held_by(f, holds) is None]
    for finding in unheld:
        problems.append(
            f"{finding['lockfile']}: {finding['package']} {finding['version']} "
            f"is affected by {finding['id']} -- {finding['summary'][:160]}. "
            "Upgrade it, or record it in "
            f"{LEDGER.relative_to(ROOT).as_posix()} with the reason and a "
            f"{EXIT_MARKERS[0]!r} naming the upstream change that retires the "
            "hold")

    # The ledger is checked in BOTH directions. A hold whose advisory no
    # longer appears is not harmless: it goes on excusing the finding and
    # would silently re-cover it if the dependency rolled back, which is the
    # direction a "what's new" reader would never think to check. Scoped to
    # the lockfiles THIS run scanned, so the uv job cannot fail over an npm
    # hold it was never in a position to see.
    scanned = {lockfile for lockfile, _ in tally}
    seen = {(h["id"], h["lockfile"]) for f in findings
            if (h := held_by(f, holds)) is not None}
    for hold in holds:
        key = (hold.get("id"), hold.get("lockfile"))
        if hold.get("lockfile") in scanned and key not in seen:
            problems.append(
                f"{LEDGER.name}: the hold on {hold['id']} "
                f"({hold.get('package')}) matches NOTHING in "
                f"{hold.get('lockfile')} any more -- the advisory is resolved "
                "or the dependency is gone. Delete the entry: a closed hold "
                "left recorded would silently re-cover the finding if it came "
                "back")

    if problems:
        print("check_advisories:\n  " + "\n\n  ".join(problems), file=sys.stderr)
        return 1

    scanned_packages = sum(n for _, n in tally)
    detail = ", ".join(f"{lockfile} ({n})" for lockfile, n in tally)
    print(f"check_advisories: {ecosystem} -- {scanned_packages} locked "
          f"packages across {len(tally)} tracked lockfiles, discovered from "
          f"`git ls-files {' '.join(ECOSYSTEMS[ecosystem])}` rather than a "
          f"hand-maintained list: {detail}. "
          f"{len(findings) - len(unheld)} advisory finding(s) accepted in "
          f"{LEDGER.name}, each with an exit condition; no unheld findings. "
          "Version-level against the advisory graph, NOT reachability-filtered "
          "the way govulncheck is.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
