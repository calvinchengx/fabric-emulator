"""Fail if a pull request carries co-authorship or AI-session trailers.

The rule, for every contributor: commits and pull request descriptions carry no
`Co-authored-by:` trailer and no `Claude-Session:` trailer. Merges go through
`make merge PR=N` (scripts/merge_pr.py), which builds the squash message from
the title and description, so the description is what lands on main. The commits
are checked too, because this repository's squash default is COMMIT_MESSAGES:
merged any other way, every commit message on the branch lands on main.

Ported from entra-emulator, where it first caught a real trailer the day it
landed. Here it came after six: squash commits for #639, #641, #642, #655, #663
and #664 (2026-10-25/26) carry `Co-authored-by:` lines that no commit on their
branches contained. GitHub appended them at merge time, one per commit author,
and those six are left in place. The push run below checks only what each push
lands (`before..after`), so it does not re-flag history it cannot change.

Matching is per line, case-insensitive, after leading whitespace, which is how
git and GitHub read trailers. A trailer name anywhere else in a line (prose that
mentions it, a code span) is not a trailer and does not match.

Usage:
    python scripts/check_attribution.py --range BASE..HEAD [--body-file FILE]

Stdlib only.
"""

import argparse
import pathlib
import re
import subprocess
import sys

TRAILER_RE = re.compile(r"^\s*(co-authored-by|claude-session)\s*:", re.IGNORECASE)


def offending_lines(text):
    """Return the lines of `text` that are forbidden trailers."""
    return [line.strip() for line in text.splitlines() if TRAILER_RE.match(line)]


def commits_in_range(rev_range):
    """Yield (sha, message) for every commit in `rev_range`."""
    out = subprocess.run(
        ["git", "log", "--format=%H%x00%B%x00", rev_range],
        check=True, capture_output=True, text=True,
    ).stdout
    fields = out.split("\x00")
    # Not strict: every record ENDS with \x00, so the split leaves one trailing
    # field (the newline git writes after the last record) with no message to
    # pair. strict=True would raise on every real range; dropping it is right.
    for sha, message in zip(fields[0::2], fields[1::2], strict=False):
        yield sha.strip(), message


def check(commits, body):
    """Return a list of human-readable findings; empty means clean."""
    findings = []
    for sha, message in commits:
        for line in offending_lines(message):
            findings.append(f"commit {sha[:12]}: {line}")
    for line in offending_lines(body or ""):
        findings.append(f"pull request description: {line}")
    return findings


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--range", required=True, help="git revision range, e.g. BASE..HEAD")
    parser.add_argument("--body-file", type=pathlib.Path, help="file holding the PR description")
    args = parser.parse_args(argv)

    body = args.body_file.read_text(encoding="utf-8") if args.body_file else ""
    commits = list(commits_in_range(args.range))
    findings = check(commits, body)
    if not findings:
        print(f"attribution: {len(commits)} commit(s) and the description carry no co-authorship trailers")
        return 0
    print("attribution: this repository does not accept Co-authored-by or Claude-Session trailers.")
    for finding in findings:
        print(f"  {finding}")
    print("Remove them: edit the description, and reword the commits"
          " (`git rebase -i`, then force-push) or squash them into one.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
