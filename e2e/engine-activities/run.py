#!/usr/bin/env python3
"""e2e: the pipeline activities that EXECUTE code, on the shipped Spark agent.

Custom (ADF Batch), HDInsightSpark and SparkJobDefinition. Their Go tests drive
a FAKE agent that records the statement it was handed, which proves dispatch and
nothing more. Here the agent is real with Sail behind it, so each activity is
judged by what came back — including that a non-zero exit FAILS the Custom
activity, which a fake cannot show.
"""
import os
import subprocess
import sys

DIR = os.path.dirname(os.path.abspath(__file__))


def compose(*a):
    return subprocess.run(["docker", "compose", *a], cwd=DIR).returncode


try:
    rc = compose("up", "--build", "--abort-on-container-exit", "--exit-code-from", "client")
    if rc != 0:
        for svc in ("client", "fabric", "spark-agent", "sail", "entra"):
            sys.stderr.write(f"\n==== {svc} logs ====\n")
            subprocess.run(["docker", "compose", "logs", "--tail", "50", svc], cwd=DIR)
    sys.exit(rc)
finally:
    compose("down", "-v")
