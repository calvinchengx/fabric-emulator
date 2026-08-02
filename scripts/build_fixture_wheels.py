#!/usr/bin/env python3
"""Build the fixture wheels a downstream repo installs, and prove they work.

WHY THIS EXISTS. `contoso-data-platform` — the standalone acceptance repo — runs
the medallion against a RELEASED emulator image, and its assertions have to be
the same numbers these examples assert. The only honest way to guarantee that is
for both repos to use ONE generator, so the data and the expectations cannot
drift. That is the same argument contoso-fixtures/pyproject.toml already makes
for the four in-tree examples, one level up: two copies that differ by a single
RNG draw would each pass their own EXPECTED_* asserts, and the comparison
between them would report green while comparing two different datasets.

THE VERSION IS STAMPED AT BUILD TIME, into a COPY. The committed pyproject.toml
files keep `version = "0.1.0"`, because the four examples depend on them by path
and their uv.lock files record that version — rewriting the committed file would
force a re-lock of four examples this script is specifically not allowed to
touch. So the version travels with the release tag without the tree moving.

WHAT IS PUBLISHED IS THE PACKAGE AS IT STANDS, `common.py` included. Not because
a downstream consumer should USE common.py — it is emulator client plumbing, and
handing it over would undercut the one claim the standalone repo exists to make,
that a consumer can build against a published image without the source. That
discipline belongs downstream, as a test asserting `common` is never imported.
Splitting it out here would mean restructuring a package four examples depend
on, which is a far larger blast radius than the problem deserves.

Usage:
    python scripts/build_fixture_wheels.py 0.11.3
    python scripts/build_fixture_wheels.py            # takes GITHUB_REF_NAME
"""
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
PACKAGES = ["contoso-fixtures", "contoso-fixtures-advanced"]
OUT = ROOT / "dist" / "fixtures"

# Asserted against the INSTALLED wheels, never the source tree — that is the
# whole point. A wheel missing a module, or carrying stale metadata, imports
# fine from a repo checkout and fails only for the consumer.
SMOKE = """
import source_system, web_store, erp_system
s, w, e = source_system, web_store, erp_system
assert s.EXPECTED_SILVER_ORDERS == 247500, s.EXPECTED_SILVER_ORDERS
assert s.EXPECTED_SILVER_CUSTOMERS == 100000, s.EXPECTED_SILVER_CUSTOMERS
assert w.EXPECTED_WEB_CLEAN_LINES == 213562, w.EXPECTED_WEB_CLEAN_LINES
assert w.EXPECTED_WEB_PRODUCTS == 8, w.EXPECTED_WEB_PRODUCTS
assert e.EXPECTED_ERP_ONLY_CURRENT == 11526, e.EXPECTED_ERP_ONLY_CURRENT
# The generators must not have been imported from a source checkout.
for m in (source_system, web_store, erp_system):
    assert "site-packages" in m.__file__, (m.__name__, m.__file__)
print("smoke ok:", s.EXPECTED_SILVER_ORDERS, w.EXPECTED_WEB_CLEAN_LINES,
      e.EXPECTED_ERP_ONLY_CURRENT)
"""


def run(cmd, **kw):
    return subprocess.run(cmd, check=True, **kw)


def version_from_argv_or_env():
    if len(sys.argv) > 1:
        v = sys.argv[1]
    else:
        v = os.environ.get("GITHUB_REF_NAME", "")
    v = v.lstrip("v")
    # Fail loudly rather than defaulting. A wheel silently stamped 0.1.0 on
    # every release is indistinguishable from one that was never rebuilt.
    if not re.fullmatch(r"\d+\.\d+\.\d+([.-][0-9A-Za-z.]+)?", v):
        sys.exit(f"need a release version, got {v!r} — "
                 f"pass it as argv[1] or set GITHUB_REF_NAME")
    return v


def main():
    version = version_from_argv_or_env()
    OUT.mkdir(parents=True, exist_ok=True)
    for f in OUT.glob("*.whl"):
        f.unlink()

    with tempfile.TemporaryDirectory() as tmp:
        tmp = pathlib.Path(tmp)
        build = tmp / "build"
        build.mkdir()

        for name in PACKAGES:
            src = ROOT / "examples" / name
            dst = build / name
            shutil.copytree(src, dst,
                            ignore=shutil.ignore_patterns(
                                "__pycache__", "*.egg-info", ".venv", "*.json"))
            pp = dst / "pyproject.toml"
            text = pp.read_text()
            stamped, n = re.subn(r'^version = "[^"]*"',
                                 f'version = "{version}"', text,
                                 count=1, flags=re.M)
            if n != 1:
                sys.exit(f"{name}: expected exactly one version line, found {n}")
            # The uv path source points at ../contoso-fixtures, which is true in
            # the tree and true in this build dir — both packages are copied.
            pp.write_text(stamped)
            run(["uv", "build", "--wheel", "--out-dir", str(OUT)], cwd=dst)

        wheels = sorted(OUT.glob("*.whl"))
        if len(wheels) != len(PACKAGES):
            sys.exit(f"expected {len(PACKAGES)} wheels, built {len(wheels)}")

        # Install BOTH together. contoso-fixtures-advanced declares
        # `contoso-fixtures` as a plain requirement — its [tool.uv.sources] path
        # is a uv-local convenience that does NOT survive into wheel metadata —
        # so resolving it from an index would fail. Downstream must install the
        # pair, and this proves the pair is sufficient.
        venv = tmp / "venv"
        run(["uv", "venv", str(venv)])
        env = {**os.environ, "VIRTUAL_ENV": str(venv)}
        run(["uv", "pip", "install", *[str(w) for w in wheels]], env=env)
        # Windows puts the interpreter in Scripts\, POSIX in bin/. This script
        # runs on a Linux runner today, but a contributor on Windows must be
        # able to run it too — that is a hard requirement for everything
        # downstream of this work.
        py = venv / ("Scripts/python.exe" if os.name == "nt" else "bin/python")
        # cwd=tmp so the repo's own modules are not importable — otherwise the
        # smoke would pass on the source tree and say nothing about the wheel.
        run([str(py), "-c", SMOKE], cwd=tmp, env=env)

    print(f"\nbuilt {len(wheels)} wheel(s) at version {version}:")
    for w in wheels:
        print(f"  {w.name}  ({w.stat().st_size:,} bytes)")


if __name__ == "__main__":
    main()
