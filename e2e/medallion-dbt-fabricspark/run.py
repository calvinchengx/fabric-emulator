#!/usr/bin/env python3
"""e2e: medallion-dbt-fabricspark, executed.

Holds no stack definition and no pipeline code. `e2e/medallion/docker-compose.yml`
is the single copy of the stack and `examples/medallion-dbt-fabricspark` is the single copy
of the pipeline; this reuses the basic harness's runner, pointed at that folder.

That reuse is only possible because the example runs on the HOST. While the
client was containerised, "same stack, different example" meant another image
and a compose overlay to swap it in.

  python3 e2e/medallion-dbt-fabricspark/run.py
"""
import os
import sys

DIR = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(DIR))
sys.path.insert(0, os.path.join(REPO, "e2e", "medallion"))

import run as basic  # noqa: E402 — after the path insert above

EXAMPLE = os.path.join(REPO, "examples", "medallion-dbt-fabricspark")

if __name__ == "__main__":
    # profiles=("livy",): this example builds silver over the Livy HC
    # surface, so it needs the statement agent the PySpark ones do not.
    sys.exit(basic.run(EXAMPLE, label="medallion-dbt-fabricspark", profiles=("livy",)))
