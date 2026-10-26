"""Squash-merge a pull request with a commit message we write, not GitHub.

GitHub's generated squash message appends `Co-authored-by:` for the pull
request's commit authors. Here that put trailers on six squash commits (#639,
#641, #642, #655, #663, #664) that no commit on their branches contained: one
for `dependabot[bot]`, and one for Calvin wherever a commit had been pushed
onto the bot's branch. In entra-emulator, where this script comes from, the
squash setting "PR title and body" did not stop it either. Passing the message
explicitly is the only way to merge without them, so this builds it (title,
` (#N)`, the description with any trailer lines removed), merges with it, and
then reads the commit that landed and fails if a trailer got in anyway.

The squash commit's AUTHOR is left to GitHub: a Dependabot pull request stays
authored by `dependabot[bot]`, which is fine. Only the trailers are the rule.

Usage:
    python scripts/merge_pr.py 233          # or: make merge PR=233

Needs `gh`, authenticated. Stdlib only.
"""

import argparse
import json
import pathlib
import subprocess
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
from check_attribution import TRAILER_RE, offending_lines  # noqa: E402


def build_message(title, number, body):
    """Return (subject, body) for the squash commit, trailers removed."""
    suffix = f" (#{number})"
    subject = title if title.endswith(suffix) else title + suffix
    kept = [line for line in (body or "").splitlines() if not TRAILER_RE.match(line)]
    return subject, "\n".join(kept).strip() + "\n"


def gh(*args):
    return subprocess.run(["gh", *args], check=True, capture_output=True, text=True).stdout


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("number", type=int, help="pull request number")
    args = parser.parse_args(argv)

    pr = json.loads(gh("pr", "view", str(args.number), "--json", "title,body,state,headRefOid"))
    if pr["state"] != "OPEN":
        print(f"merge: #{args.number} is {pr['state'].lower()}, not open", file=sys.stderr)
        return 1
    subject, body = build_message(pr["title"], args.number, pr["body"])
    # --match-head-commit: refuse if someone pushed after we read the description.
    subprocess.run(["gh", "pr", "merge", str(args.number), "--squash", "--delete-branch",
                    "--match-head-commit", pr["headRefOid"], "--subject", subject, "--body-file", "-"],
                   input=body, text=True, check=True)

    sha = json.loads(gh("pr", "view", str(args.number), "--json", "mergeCommit"))["mergeCommit"]["oid"]
    landed = json.loads(gh("api", f"repos/{{owner}}/{{repo}}/commits/{sha}"))["commit"]["message"]
    found = offending_lines(landed)
    if found:
        print(f"merge: {sha[:12]} landed WITH trailers despite the explicit message:", file=sys.stderr)
        for line in found:
            print(f"  {line}", file=sys.stderr)
        return 1
    print(f"merge: #{args.number} landed as {sha[:12]} with no co-authorship trailers")
    return 0


if __name__ == "__main__":
    sys.exit(main())
