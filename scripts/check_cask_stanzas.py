# /// script
# requires-python = ">=3.12"
# dependencies = ["pyyaml"]
# ///
"""Fail if the Homebrew cask we publish would use a deprecated stanza.

Homebrew deprecated `preflight`, `postflight`, `uninstall_preflight` and
`uninstall_postflight` in favour of the `_steps` forms. The cask is generated,
so the deprecation cannot be fixed in the tap alone: GoReleaser's template
hardcodes `postflight do` around whatever `homebrew_casks[].hooks` contains, and
the next release would undo any hand-edit. The fix is to carry the stanza in
`custom_block`, which the template emits verbatim.

Two checks, because they fail at different times and catch different things:

  (default)      the SOURCE: `.goreleaser.yaml` must not use `hooks`, since any
                 hook at all renders as a deprecated block. Runs on every push,
                 which is where a regression would be introduced.
  --rendered DIR the OUTPUT: the cask GoReleaser actually wrote under
                 dist/homebrew/. Runs at release time, and is the check that
                 survives GoReleaser changing its template underneath us.

Usage:
    uv run --python 3.12 scripts/check_cask_stanzas.py
    uv run --python 3.12 scripts/check_cask_stanzas.py --rendered dist/homebrew
"""

import argparse
import pathlib
import re
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parent.parent

# Every stanza Homebrew deprecated in favour of a `_steps` form. Matched as a
# whole word followed by `do`, so `postflight_steps do` does not match.
DEPRECATED = ("preflight", "postflight", "uninstall_preflight", "uninstall_postflight")
DEPRECATED_RE = re.compile(
    r"^\s*(" + "|".join(DEPRECATED) + r")\s+do\b",
    re.MULTILINE,
)


def config_path() -> pathlib.Path:
    for name in (".goreleaser.yaml", ".goreleaser.yml"):
        p = ROOT / name
        if p.exists():
            return p
    sys.exit("no .goreleaser.yaml found")


def check_source() -> list[str]:
    cfg = yaml.safe_load(config_path().read_text()) or {}
    problems = []
    for cask in cfg.get("homebrew_casks") or []:
        name = cask.get("name", "<unnamed>")
        if cask.get("hooks"):
            problems.append(
                f"{name}: homebrew_casks[].hooks renders as a deprecated "
                f"`preflight`/`postflight` block. Move the Ruby into "
                f"`custom_block` as a `_steps` stanza instead."
            )
        block = cask.get("custom_block") or ""
        for m in DEPRECATED_RE.finditer(block):
            problems.append(
                f"{name}: custom_block opens a deprecated `{m.group(1)}` block; "
                f"use `{m.group(1)}_steps`."
            )
    return problems


def check_rendered(directory: pathlib.Path) -> list[str]:
    casks = sorted(directory.rglob("*.rb"))
    if not casks:
        # A silent zero here would be the whole point of the check evaporating:
        # no files scanned reads exactly like no problems found.
        return [f"no cask was found under {directory}; nothing was checked"]
    problems = []
    for cask in casks:
        for m in DEPRECATED_RE.finditer(cask.read_text()):
            problems.append(f"{cask}: uses deprecated `{m.group(1)}`")
    print(f"scanned {len(casks)} rendered cask(s) under {directory}")
    return problems


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--rendered", type=pathlib.Path,
                    help="directory of casks GoReleaser rendered (dist/homebrew)")
    args = ap.parse_args()

    problems = check_rendered(args.rendered) if args.rendered else check_source()
    if problems:
        print("Deprecated Homebrew cask stanzas:")
        for p in problems:
            print(f"  {p}")
        return 1
    print("no deprecated cask stanzas")
    return 0


if __name__ == "__main__":
    sys.exit(main())
