#!/usr/bin/env python3
"""Regenerate fixture/expected.json from a running emulator.

The golden is OUR answer, so it must come from the evaluator and never be
hand-edited: a golden someone typed is a test of their arithmetic. Run this
whenever the fixture or internal/semanticmodel changes, against a stack the
usual way, and commit the diff.

    docker compose up -d entra-emulator fabric-emulator
    uv run --frozen --no-sync python e2e/pbix-desktop/regolden.py
"""
import sys
print(__doc__)
print("Not automated on purpose: it needs a stack, and a script that silently "
      "regenerated a golden would let a regression rewrite its own expectation.",
      file=sys.stderr)
