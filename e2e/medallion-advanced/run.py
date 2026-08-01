#!/usr/bin/env python3
"""e2e: the ADVANCED medallion, executed.

Same stack, different example. This harness holds no stack definition and no
pipeline code: `e2e/medallion/docker-compose.yml` is the single copy of one and
`examples/medallion-advanced-pyspark` is the single copy of the other. It reuses
the basic harness's runner outright, pointed at the advanced folder.

That reuse is only possible because the example runs on the HOST. While the
client was containerised, "same stack, different example" meant a second image
and a compose overlay to swap it in — machinery that existed solely to put
Python somewhere it did not need to be.

  python3 e2e/medallion-advanced/run.py

Same requirements as the basic harness: Microsoft ODBC Driver 18 on the host,
Linux weight class.
"""
import os
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))
sys.path.insert(0, os.path.join(REPO, "e2e", "medallion"))

import run as basic  # noqa: E402 — after the path insert above

EXAMPLE = os.path.join(REPO, "examples", "medallion-advanced-pyspark")

if __name__ == "__main__":
    sys.exit(basic.run(EXAMPLE, label="advanced medallion"))
