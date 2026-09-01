"""Submit a Java/Scala main class to the JVM overlay, as `spark-submit` does.

WHY THIS EXISTS. Two activities ask the emulator to EXECUTE a named main class
— a Databricks JAR task (`mainClassName` + a jar) and an HDInsight MapReduce
job (`className` + `jarFilePath`). Both were refused by name, with the same
cause: "the agent runs Python statements and nothing here submits a main class,
on either engine". The second half of that was true and the first half was a
gap rather than a law — the overlay image IS Apache Spark and ships
`spark-submit`; nothing had wired it up.

WHAT IT DOES NOT PRETEND TO BE. This is not a Databricks cluster and not a YARN
MapReduce runtime. It is `spark-submit --class` on the engine the emulator
already runs, which is exactly what a JAR task means and what MapReduce does
NOT: a MapReduce job is a mapper/reducer contract, and running its jar through
spark-submit would execute something else and call it the same thing. So the
MapReduce refusal keeps its cause and only its wording changes — see
unrunnableactivities.go.

SAIL HAS NO spark-submit, and that is the honest boundary: `available()` looks
for the binary rather than assuming, so an emulator on the default engine
refuses with a reason instead of failing obscurely half a second later.
"""

import os
import shutil
import subprocess

# Where a submitted jar is staged. Inside the agent's container, beside the
# lakehouse mount rather than in /tmp, so a jar and the data it reads are on
# the same filesystem the notebook sees.
STAGE = "/lakehouse/default/.submit"


# Where the overlay image puts spark-submit. A literal rather than
# os.environ.get("SPARK_HOME", …): nothing in this stack sets SPARK_HOME, so
# reading it would offer a knob that is never turned and hide that the fallback
# is the only path anyone travels — which is what check_env_documented caught
# when this file first read it.
SPARK_BIN = "/opt/spark/bin/spark-submit"


def available():
    """The path to spark-submit, or "" when this engine has none (Sail)."""
    if os.path.isfile(SPARK_BIN) and os.access(SPARK_BIN, os.X_OK):
        return SPARK_BIN
    return shutil.which("spark-submit") or ""


def submit(main_class, jar_path, args=None, conf=None, timeout=900):
    """Run one main class and report what the JVM did.

    The EXIT CODE decides, not the absence of an exception: a spark-submit that
    returns non-zero has failed even when it printed nothing, and reporting
    success there would be the fabrication this repo exists to avoid.
    """
    binary = available()
    if not binary:
        return {"ok": False, "available": False, "exitCode": None,
                "error": "this engine has no spark-submit, so a main class cannot be "
                         "executed here; the JVM overlay provides one"}
    if not main_class:
        return {"ok": False, "available": True, "exitCode": None,
                "error": "mainClass is required"}
    if not jar_path or not os.path.isfile(jar_path):
        return {"ok": False, "available": True, "exitCode": None,
                "error": f"the jar was not staged at {jar_path!r}"}

    cmd = [binary, "--class", main_class]
    for key, value in (conf or {}).items():
        cmd += ["--conf", f"{key}={value}"]
    cmd += [jar_path] + [str(a) for a in (args or [])]

    try:
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return {"ok": False, "available": True, "exitCode": None,
                "error": f"spark-submit did not finish within {timeout}s"}

    tail = (proc.stdout or "")[-8192:], (proc.stderr or "")[-8192:]
    return {"ok": proc.returncode == 0, "available": True,
            "exitCode": proc.returncode, "stdout": tail[0], "stderr": tail[1],
            "error": "" if proc.returncode == 0 else
                     f"spark-submit exited {proc.returncode}"}
