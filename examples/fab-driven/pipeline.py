#!/usr/bin/env python3
"""Run every step, in order.

Same shape as the other examples: the order lives here and nowhere else, steps
are named for what they do rather than numbered, and each is an ordinary script
you can run by hand. This runner executes them as separate processes, exactly as
a reader would, so nothing passes here that would fail when typed one line at a
time.
"""
import pathlib
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent

STEPS = [
    ("provision", "fab mkdir: a workspace on a capacity, and a lakehouse"),
    ("land", "fab cp: the vendor export into OneLake Files"),
    ("import_items", "fab import: real definitions, then exported back and diffed"),
    ("run", "fab job run: the pipeline on the emulator, the notebook on Spark"),
    ("readback", "fab ls + an independent Delta reader agree on what landed"),
]


def main() -> int:
    for i, (step, title) in enumerate(STEPS, 1):
        print(f"==> [{i}/{len(STEPS)}] {title}", flush=True)
        rc = subprocess.run([sys.executable, f"{step}.py"], cwd=HERE).returncode
        if rc != 0:
            sys.exit(f"FAILED at {step}.py (exit {rc})")
    print(f"==> fab-driven complete: {len(STEPS)}/{len(STEPS)} steps passed", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
