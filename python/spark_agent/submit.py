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

# The only directory a jar may be run from: the lakehouse mount, which is where
# the emulator puts it and the one place the agent can justify reading.
MOUNT_ROOT = "/lakehouse/default/Files"


# Where the overlay image puts spark-submit. A literal rather than
# os.environ.get("SPARK_HOME", …): nothing in this stack sets SPARK_HOME, so
# reading it would offer a knob that is never turned and hide that the fallback
# is the only path anyone travels — which is what check_env_documented caught
# when this file first read it.
SPARK_BIN = "/opt/spark/bin/spark-submit"


def _resolve_in_mount(requested):
    """The jar the caller asked for, chosen from the files that ACTUALLY EXIST
    under the mount — or None.

    THE PATH HANDED TO spark-submit IS NEVER BUILT FROM THE REQUEST. The mount
    is enumerated here, and the request only SELECTS among what the walk found;
    a value that matches nothing is refused. So there is no path expression
    derived from caller data to get wrong, which is the difference between
    sanitising untrusted input and not using it as a path at all.

    That is the same move the AKV SSRF argument reaches for: prefer the trusted
    value over a validated copy of the untrusted one. It also strictly
    outperforms a containment check — a traversal, an absolute path elsewhere,
    and a symlink pointing out of the mount all simply fail to match anything
    the walk produced.
    """
    if not requested:
        return None
    root = os.path.realpath(MOUNT_ROOT)
    if not os.path.isdir(root):
        return None

    # What the caller means, reduced to a mount-relative form for comparison.
    #
    # SEPARATORS ARE NORMALISED ON BOTH SIDES. The agent only ever runs in a
    # Linux container, but its tests run on the Windows runner too, and there
    # `os.path.relpath` returns `jobs\etl.jar` while the request carries
    # `jobs/etl.jar` — so the walk matched nothing and every jar looked absent.
    # A comparison that depends on the host's separator is wrong even where it
    # happens to work.
    def rel(path):
        return path.replace("\\", "/").strip("/")

    wanted = rel(str(requested))
    root_rel = rel(root)
    if wanted.startswith(root_rel + "/"):
        wanted = wanted[len(root_rel) + 1:]

    for dirpath, _dirnames, filenames in os.walk(root):
        for name in filenames:
            found = os.path.join(dirpath, name)
            if rel(os.path.relpath(found, root)) == wanted:
                return found
    return None


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
    contained = _resolve_in_mount(jar_path)
    if contained is None:
        # THE AGENT IS A SERVICE, NOT A LIBRARY. Whatever the emulator checks
        # before calling, this path arrives over HTTP and is only as trusted as
        # the port. Without this, `jar: "/lakehouse/default/Files/../../opt/x.jar"`
        # — or any absolute path — would be handed to spark-submit, which would
        # happily load a jar from anywhere in the container. CodeQL called it
        # py/path-injection and was right.
        #
        # Same containment files_mount.py applies to writes, for the same
        # reason and by the same means: realpath first, so a symlink already
        # inside the mount is not an escape either, and commonpath rather than
        # a string prefix, because `/lakehouse/default/Files-evil` starts with
        # the root as a string while being a different directory.
        return {"ok": False, "available": True, "exitCode": None,
                "error": f"no jar matching {jar_path!r} exists under {MOUNT_ROOT} — a jar is run "
                         "from the lakehouse mount, and a path that names nothing there is "
                         "refused rather than submitted. A traversal, an absolute path "
                         "elsewhere, or a symlink out of the mount all land here, because the "
                         "mount is enumerated and the request only selects from it"}
    jar_path = contained

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
