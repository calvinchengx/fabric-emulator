"""The child process entry point: `python usercontext_child.py`.

SEPARATED FROM usercontext.py TO REMOVE THE LAST EDGE OF THIS REPOSITORY'S ONLY
IMPORT CYCLE. `agent` imports `usercontext` at module scope, and `usercontext`
imported `agent` back -- twice. One of those was per-statement and is gone
(run_code now lives in codeexec). The other is THIS function, and it cannot go
away: the child genuinely needs the agent module, because `agent.ns` builds the
per-session SparkSession it runs user code in.

So the module that needs both moves out of the library. usercontext.py is now
imported-only and imports agent never; this file, which nothing imports, is
free to import both. A module that is both a library and a __main__ is what put
the cycle there.

The image ships it: docker/spark-runtime/Dockerfile copies
`python/spark_agent/*.py`, so a new sibling arrives without a manifest edit.
"""

import sys

import usercontext


def main():  # pragma: no cover - exercised as a subprocess, not in-process
    # Opened BEFORE importing agent, which brings up Spark: the descriptor must
    # be claimed while we still know it is ours, not after a library has had a
    # chance to touch the table. This ordering is why the import is here and not
    # at module scope, and it survives the move unchanged.
    responses = usercontext.protocol_stream()
    import agent

    with responses:
        usercontext.serve(sys.stdin.buffer, responses, usercontext._dispatch,
                          lambda: agent.ns("child"))


if __name__ == "__main__":  # pragma: no cover
    main()
