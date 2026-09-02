#!/usr/bin/env python3
"""Measure where a JVM Spark Connect client stops working against Sail.

WHY THIS EXISTS. `DatabricksSparkJar` runs on the JVM overlay only, because the
default engine (Sail) has no `spark-submit`. But Sail is a Connect *server*, and
Apache publishes a JVM Connect *client* that needs no cluster — so a jar's main
class can run on a real JVM while Sail computes. This probe is the evidence for
that claim, and the alarm for the day it changes.

THE REFUSALS ARE THE POINT. Four things need a JVM on the SERVER side, which
Sail (Rust) does not have. They are asserted to REFUSE. If one starts working,
this run FAILS — loudly, saying the boundary moved — because that is when the
runner's pre-flight checks and the parity map are wrong, and the previous way to
find out was for a user to hit it. `scripts/probe_sail_merge_premise.py` exists
because the MERGE intercept's premise expired silently for a whole release; this
is the same lesson, wired to a schedule instead of to a memory.

WHAT IT DOES NOT SETTLE. It runs against a bare local Sail writing parquet to a
temp directory. The emulator's real path is OneLake over `abfss://` inside the
compose stack, and docs/20 records storage URLs as exactly what has separated
Sail behaviours before. A green run here is necessary for the runner and nowhere
near sufficient.
"""

import os
import re
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
sys.path.insert(0, os.path.join(ROOT, "scripts"))

# The exit code the probe is told to end with. Arbitrary and non-zero: the
# activity reports success from a jar's exit code, so "the code propagates" is
# part of the contract, not a detail. 0 would pass by accident.
EXIT_CODE = 7

# ENUMERATE THE GOOD, REFUSE THE REST. A case that reports anything other than
# its expected outcome fails the run, and so does a case that appears here but
# not in the output, or in the output but not here. There is no bucket for
# "unrecognised", because that is the bucket unknown states hide in.
EXPECTED = {
    "handshake_sql": "ran",
    "dataframe_ops": "ran",
    "parquet_roundtrip": "ran",
    "sql_ddl_view": "ran",
    "args_passed": "ran",
    "exit_code": "ran",
    "typed_dataset_map": "refused",
    "spark_context": "refused",
    "scala_udf": "refused",
    "add_artifact": "refused",
}

# What each refusal SAID on 2026-09-02 (Spark 4.1.2 client, Sail 0.7.1). Drift
# here is reported, never fatal: the design depends on the outcome, not the
# wording. It is recorded because two of these are unreadable — "wildcard with
# plan ID" and a bare gRPC INTERNAL name nothing a user could act on — and that
# is the argument for the runner pre-scanning jars instead of letting them fail
# like this. If Sail ever answers clearly, part of that scan can go.
RECORDED = {
    "typed_dataset_map": "wildcard with plan ID",
    "spark_context": "UNSUPPORTED_CONNECT_FEATURE.SESSION_SPARK_CONTEXT",
    "scala_udf": "Scala UDF is not supported yet",
    "add_artifact": "handle add artifacts",
}

CASE = re.compile(r"^CASE (\S+) (ran|refused) :: (.*)$")


def require(tool: str) -> str:
    """A missing JDK FAILS. It must not skip: a skipped probe reports success,
    and this one exists to contradict an assumption, so silence is the worst
    possible answer (see docs/20 on guards that skip rather than fail)."""
    found = shutil.which(tool)
    if not found:
        sys.exit(f"FATAL: no `{tool}` on PATH. This probe needs a JDK 17+; "
                 "install one (CI uses actions/setup-java) rather than skipping.")
    return found


def main() -> int:
    javac, java = require("javac"), require("java")

    import fetch_connect_client_jars as fetcher
    from pysail.spark import SparkConnectServer

    classpath = os.pathsep.join(fetcher.fetch(os.path.join(HERE, "jars")))
    work = tempfile.mkdtemp(prefix="sail-jvm-client-")
    classes = os.path.join(work, "classes")

    compiled = subprocess.run([javac, "-cp", classpath, "-d", classes,
                               os.path.join(HERE, "Probe.java")],
                              capture_output=True, text=True)
    if compiled.returncode != 0:
        print(compiled.stdout + compiled.stderr)
        return 1

    server = SparkConnectServer(port=0)
    server.start()
    _, port = server.listening_address
    print(f"sail listening on {port}; running the probe")

    sentinel = "sentinel-9f3c"
    proc = subprocess.run(
        [java,
         # Arrow reaches into java.nio, and Spark's own launcher scripts pass
         # the same opens. Without them the client dies before the handshake.
         "--add-opens=java.base/java.nio=ALL-UNNAMED",
         "--add-opens=java.base/java.lang=ALL-UNNAMED",
         "--add-opens=java.base/java.util=ALL-UNNAMED",
         "-cp", classpath + os.pathsep + classes, "Probe",
         "--exit-with", str(EXIT_CODE),
         "--artifact", classpath.split(os.pathsep)[0],
         "--warehouse", work,
         "--sentinel", sentinel],
        capture_output=True, text=True,
        env={**os.environ, "SPARK_REMOTE": f"sc://localhost:{port}"})

    observed, failures, notes = {}, [], []
    for line in proc.stdout.splitlines():
        m = CASE.match(line)
        if m:
            observed[m.group(1)] = (m.group(2), m.group(3))

    for case, want in EXPECTED.items():
        if case not in observed:
            failures.append(f"{case}: never reported (the probe died before it, or was renamed)")
            continue
        got, detail = observed[case]
        if got != want:
            moved = "THE BOUNDARY MOVED: " if want == "refused" else ""
            failures.append(f"{case}: expected {want}, got {got} — {moved}{detail}")
        elif want == "refused" and RECORDED.get(case, "") not in detail:
            notes.append(f"{case}: refusal message changed\n"
                         f"      recorded: {RECORDED.get(case, '')}\n"
                         f"      now:      {detail}")
        print(f"  {case:<20} {got:<8} {detail[:90]}")

    for case in set(observed) - set(EXPECTED):
        failures.append(f"{case}: reported but not expected — add it to EXPECTED deliberately")

    if proc.returncode != EXIT_CODE:
        failures.append(f"exit code {proc.returncode}, expected {EXIT_CODE}: a jar's exit "
                        "code does not reach the caller, so nothing can report on it")
    if proc.returncode != 0 and not observed:
        print(proc.stdout + proc.stderr)

    for note in notes:
        print(f"NOTE  {note}")
    if failures:
        print("\nFAILED:")
        for f in failures:
            print(f"  - {f}")
        return 1
    print(f"\nOK: {len(EXPECTED)} cases as expected, exit code {proc.returncode} propagated")
    return 0


if __name__ == "__main__":
    sys.exit(main())
