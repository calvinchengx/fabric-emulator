#!/usr/bin/env python3
"""Names the notebook runtime READS that nothing in the product ever SETS.

WHY THIS EXISTS. Four separate defects in this repository had one shape: a
member was implemented, unit-tested and green, and did not work — because the
test supplied a value the product never produced.

  * `notebookutils.session.stop` / `restartPython` read `SPARK_AGENT_URL` and
    `FABRIC_SESSION_ID` from the environment. Nothing in the tree set either,
    so both raised "this is running outside a notebook session" INSIDE one.
    The unit tests passed throughout, because they set the variables.
  * `notebookutils.nbResPath` resolved `builtin/` against
    `context["rootNotebookId"]`. Nothing ever sent one, so a referenced child
    read its OWN resources — the divergence the module was written to prevent.
    Its unit test set the key on a stubbed context.

Both were found by writing an end-to-end witness, one at a time, by hand. This
turns that into a check: every environment variable and every runtime-context
key the shim or the agent READS must be SET somewhere that is not the reading
module and not a test.

WHAT IT CANNOT DO, said plainly. Finding readers is exact — it is an AST walk.
Finding WRITERS is not: a value can arrive from a compose file, a Dockerfile,
Go code, a shell script or another Python module, and this looks for assignment
shapes in each. A writer spelled some way this does not recognise shows up as a
false positive, which costs a waiver; a coincidental match hides a real one,
which costs a defect. So the writer patterns are deliberately narrow — an
assignment, not a mention — and a name appearing only in prose or a comment
does NOT count as set.

WAIVERS EXPIRE. `--strict` fails on a waived name that is no longer a problem,
for the same reason `check_notebookutils_surface` does: two of ITS waivers had
gone stale before anyone reread them, and a waiver nobody rechecks is how a
defect becomes furniture.

Usage:
    check_runtime_wiring.py            report
    check_runtime_wiring.py --strict   exit non-zero on any problem
"""
import argparse
import ast
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent

# The runtime a notebook actually runs in: the shim it imports and the agent
# that executes its cells. Both read values somebody else must provide.
READERS = [ROOT / "python" / "notebookutils", ROOT / "python" / "spark_agent"]

SKIP_DIRS = {"__pycache__", "build", ".venv", "node_modules", ".git"}

# Waived, with the reason. Each says why the name is read with nothing setting
# it, and each is checked for still being true.
WAIVED = {
    "SPARK_AGENT_URL": (
        "documented escape hatch for driving the agent directly, and the "
        "agent's own default when it is not set. The real path binds it per "
        "statement (notebookutils/session.py), which is what the environment "
        "could not do: one agent serves many concurrent sessions and a process "
        "variable would give every one of them the same value"),
    "FABRIC_SESSION_ID": (
        "same escape hatch as SPARK_AGENT_URL, for the session id. Superseded "
        "by the per-statement binding for exactly the reason above"),
    "LIVY_SESSION_ID": ("second spelling of FABRIC_SESSION_ID, for a caller "
                        "driving the Livy layer directly"),
    "NOTEBOOKUTILS_AGENT_URL": ("second spelling of SPARK_AGENT_URL"),
}


class Unreadable(Exception):
    """The tree is not the shape this reads."""


def _py_files(root: pathlib.Path):
    for path in sorted(root.rglob("*.py")):
        if any(part in SKIP_DIRS for part in path.parts):
            continue
        yield path


def env_reads(path: pathlib.Path) -> set:
    """Environment names this file READS. An AST walk, so it is exact."""
    names = set()
    tree = ast.parse(path.read_text(encoding="utf-8"))
    for node in ast.walk(tree):
        # os.environ.get("X"), os.getenv("X"), environ.get("X"), getenv("X").
        # The two spellings have DIFFERENT receivers — `os.environ` for the
        # first, plain `os` for the second — and treating them alike missed
        # every `os.getenv` in the tree.
        if isinstance(node, ast.Call) and node.args:
            arg = node.args[0]
            if not (isinstance(arg, ast.Constant) and isinstance(arg.value, str)):
                continue
            fn = node.func
            attr_read = (isinstance(fn, ast.Attribute)
                         and (fn.attr == "getenv"
                              or (fn.attr == "get" and _is_environ(fn.value))))
            bare_read = isinstance(fn, ast.Name) and fn.id == "getenv"
            if attr_read or bare_read:
                names.add(arg.value)
        # os.environ["X"] — a read only when it is not the assignment target,
        # which ast.Store on the subscript tells us.
        if (isinstance(node, ast.Subscript) and _is_environ(node.value)
                and isinstance(node.slice, ast.Constant)
                and isinstance(node.slice.value, str)
                and not isinstance(node.ctx, ast.Store)):
            names.add(node.slice.value)
    return names


def _is_environ(node) -> bool:
    """`os.environ`, or the bare `environ`."""
    if isinstance(node, ast.Attribute) and node.attr == "environ":
        return True
    return isinstance(node, ast.Name) and node.id == "environ"


def context_reads(path: pathlib.Path) -> set:
    """Runtime-context keys this file READS: `<something>context.get("k")`."""
    keys = set()
    tree = ast.parse(path.read_text(encoding="utf-8"))
    for node in ast.walk(tree):
        if not (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)):
            continue
        if node.func.attr != "get" or not node.args:
            continue
        target = node.func.value
        name = getattr(target, "id", None) or getattr(target, "attr", None) or ""
        if not name.lower().endswith("context") and name not in ("ctx",):
            continue
        arg = node.args[0]
        if isinstance(arg, ast.Constant) and isinstance(arg.value, str) and arg.value:
            keys.add(arg.value)
    return keys


# A WRITER, not a mention. Each pattern is an assignment in the language it
# belongs to; `NAME` is substituted in.
WRITER_PATTERNS = (
    r'^\s*{n}\s*:',                 # compose / any YAML mapping entry
    r'\bENV\s+{n}[\s=]',            # Dockerfile
    r'os\.Setenv\(\s*"{n}"',        # Go
    r'"{n}"\s*:',                   # Go map literal, Python dict, JSON
    r'environ\[\s*["\']{n}["\']\s*\]\s*=',   # Python assignment
    r'^\s*(?:export\s+)?{n}=',      # shell
    r'\b{n}\s*=\s*[^=]',            # env: NAME=value in a compose list, make
    r'\(\s*["\']{n}["\']\s*,',   # ("destKey", "srcKey") — the agent's context mapping
)

# THIS FILE NAMES EVERY NAME IT CHECKS, in WAIVED and in the docstring, and
# `"NAME":` is one of the writer patterns — so without this the check reported
# its own waiver list as the thing that sets them, and every waiver read as
# stale. A checker that satisfies its own condition is the most embarrassing
# kind of false green, and it is precisely the defect class this file is for.
SELF = pathlib.Path(__file__).resolve()



def _is_test(path: pathlib.Path, allow_e2e: bool = False) -> bool:
    """Test code, in any of the shapes this repository uses.

    A TEST IS NEVER A PRODUCER, and getting this wrong made the check useless
    in the most on-the-nose way available: `rootNotebookId` was credited to
    `internal/api/notebook_reference_run_test.go`, so removing the agent's
    wiring left the check green — a test supplying what the product does not,
    which is the entire defect class this file exists to find.

    Filenames, not just directories: Go tests live beside the package they test
    and never under a `tests/` folder, and the first version only excluded the
    folder.
    """
    if "tests" in path.parts:
        return True
    if "e2e" in path.parts and not allow_e2e:
        return True
    name = path.name
    return (name.endswith("_test.go") or name.endswith("_test.py")
            or name.startswith("test_") or name == "conftest.py")


_CORPUS = None


def _corpus():
    """Every candidate file's lines, read ONCE.

    `writers` is called per name, and re-walking the repository each time made
    the unit suite take 38 seconds for a check that reads a few hundred files.
    A slow gate is one people learn to skip.
    """
    global _CORPUS
    if _CORPUS is None:
        _CORPUS = []
        for path in sorted(ROOT.rglob("*")):
            if not path.is_file() or any(part in SKIP_DIRS for part in path.parts):
                continue
            if path.suffix == ".md" or path.resolve() == SELF:
                continue
            try:
                text = path.read_text(encoding="utf-8")
            except (UnicodeDecodeError, OSError):
                continue
            # Comments are dropped here rather than per name: a mention in a
            # comment is someone writing the name down, not something providing
            # it.
            lines = [ln for ln in text.splitlines()
                     if not ln.strip().startswith(("#", "//", "*"))]
            _CORPUS.append((path, lines))
    return _CORPUS


def writers(name: str, exclude: set, allow_e2e: bool = False) -> list:
    """Files that plausibly SET `name`, excluding the files that READ it.

    Only the readers OF THIS NAME are excluded, not the whole runtime tree.
    The agent setting a value the shim reads IS the wiring — that is the
    normal, correct arrangement — and excluding the agent wholesale reported
    `FABRIC_JOB_ID` as unset when `spark_agent/storage.py` exports it. Found by
    running this check against a tree already known to be wired, which is the
    only way a producer scan gets calibrated.
    """
    found = []
    compiled = [re.compile(p.format(n=re.escape(name))) for p in WRITER_PATTERNS]
    for path, lines in _corpus():
        if path in exclude or _is_test(path, allow_e2e):
            continue
        for line in lines:
            if name in line and any(rx.search(line) for rx in compiled):
                found.append(str(path.relative_to(ROOT)))
                break
    return found


def collect():
    reader_files, env, ctx = set(), {}, {}
    for root in READERS:
        if not root.is_dir():
            raise Unreadable(f"{root} is missing; this check has nothing to read.")
        for path in _py_files(root):
            reader_files.add(path)
            for n in env_reads(path):
                env.setdefault(n, set()).add(str(path.relative_to(ROOT)))
            for k in context_reads(path):
                ctx.setdefault(k, set()).add(str(path.relative_to(ROOT)))
    if not env:
        raise Unreadable(
            "no environment reads found at all — the AST walk matched nothing, "
            "so this check is vacuous rather than clean.")
    return reader_files, env, ctx


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--strict", action="store_true")
    args = parser.parse_args(argv)

    _reader_files, env, ctx = collect()
    problems, stale = [], []

    def unwired(index, allow_e2e):
        # Exclude the readers OF THIS NAME, resolved back to paths.
        return {n: files for n, files in index.items()
                if not writers(n, {ROOT / f for f in files}, allow_e2e)}

    # AN ENVIRONMENT NAME AND A CONTEXT KEY ARE NOT THE SAME KIND OF THING, and
    # the e2e tree is where that shows. An env var is CONFIGURATION: anyone
    # running the stack can set it, and a compose file under e2e/ demonstrating
    # one is a legitimate answer to "does anything provide this". A context key
    # is RUNTIME STATE: only the product can produce it, so a harness setting
    # one proves nothing about a real run — which is how `rootNotebookId`
    # looked wired while nothing in the product sent it.
    orphan_env = unwired(env, allow_e2e=True)
    for name in sorted(orphan_env):
        if name in WAIVED:
            continue
        problems.append(
            f"{name} is read by {', '.join(sorted(orphan_env[name]))} and set nowhere. "
            "A notebook meets the fallback, never the value.")
    for name in sorted(WAIVED):
        if name not in env:
            stale.append(f"WAIVED lists {name}, which nothing reads any more. Drop it.")
        elif name not in orphan_env:
            stale.append(f"WAIVED lists {name}, which is now set. Drop it.")

    orphan_ctx = unwired(ctx, allow_e2e=False)
    for key in sorted(orphan_ctx):
        problems.append(
            f'context key "{key}" is read by {", ".join(sorted(orphan_ctx[key]))} '
            "and set nowhere. It resolves to the fallback in every real run.")

    print("notebook runtime wiring (what the shim and the agent read):")
    print(f"  environment names read : {len(env)}")
    print(f"  context keys read      : {len(ctx)}")
    print(f"  waived                 : {len(WAIVED)}")
    print(f"  unwired                : {len(problems)}")
    for line in problems + stale:
        print(f"  {line}")
    if not problems and not stale:
        print("  every name a notebook depends on is provided by something.")
    if args.strict and (problems or stale):
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
