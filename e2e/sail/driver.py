#!/usr/bin/env python3
"""S0 driver: real PySpark (Spark Connect client, no JVM anywhere) against
Sail, writing and reading Delta through fabric-emulator's OneLake plane.

Control plane: create workspace + lakehouse with a fabric-audience token.
Data plane: Sail executes the plans; its object_store speaks the az:// +
endpoint-override recipe to the emulator's Blob surface, including the
If-None-Match conditional PUT every Delta commit needs.
"""
import faulthandler
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor

FABRIC = os.environ["FABRIC"]
ENTRA = os.environ["ENTRA"]
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"

# --- hang budget ------------------------------------------------------------
#
# This suite hung once for its CI job's entire 25-minute budget and was reported
# as CANCELLED. That is worse than a failure: a cancelled check reads as
# infrastructure noise, so it gets rerun rather than investigated — which is
# what happened, and the rerun went green.
#
# Two things have to be true for the next hang to be legible, and neither was.
# Progress must be visible AS IT HAPPENS: python block-buffers stdout to a pipe,
# so in the run that PASSED all twenty prints below carry a single timestamp,
# flushed at exit — a killed run prints nothing, and the silence locates
# nothing. `PYTHONUNBUFFERED=1` in docker/python-runtime/Dockerfile fixes that
# for every driver on that image. And a stuck step must end the run itself,
# naming what it was waiting on, well inside the job's budget: every Spark
# Connect call below is a gRPC round trip with no deadline, so nothing here is
# bounded on its own.
#
# Steps take well under a second in practice, including on a cold CI runner, so
# the budget is loose enough to never fire on slowness alone.
STEP_BUDGET = float(os.environ.get("SAIL_E2E_STEP_BUDGET", "180"))
HEARTBEAT = 30.0

_progress = threading.Lock()
_step = "startup"
_step_began = time.monotonic()


def step(name):
    """Name the operation now in flight, and restart its budget."""
    global _step, _step_began
    with _progress:
        _step, _step_began = name, time.monotonic()
    print(f"--> {name}", flush=True)


def _watchdog():
    next_beat = HEARTBEAT
    while True:
        time.sleep(1.0)
        with _progress:
            name, elapsed = _step, time.monotonic() - _step_began
        # The budget is checked BEFORE the heartbeat gate, and not after it: a
        # `continue` for "nothing to report yet" also skipped the budget, so any
        # budget under one heartbeat was unreachable and the guard passed a run
        # it was set to kill. A watchdog that cannot be made to fire is not
        # evidence of anything.
        if elapsed > STEP_BUDGET:
            print(f"\nHUNG: {elapsed:.0f}s with no progress, waiting on: {name}",
                  flush=True)
            # Every thread, so a stuck concurrent writer is as visible as a
            # stuck main thread. gRPC drops the GIL while it waits, so this
            # thread still runs when the others are blocked on the wire.
            faulthandler.dump_traceback()
            sys.stderr.flush()
            os._exit(1)
        if elapsed < next_beat:  # a new step started; re-arm the heartbeat
            next_beat = HEARTBEAT
            continue
        print(f"    still {name} ({elapsed:.0f}s)", flush=True)
        next_beat = elapsed + HEARTBEAT


threading.Thread(target=_watchdog, daemon=True).start()


def post_json(url, body, token=None):
    req = urllib.request.Request(url, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read() or b"{}")


def entra_token(scope):
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": "00d88624-f0d7-46f6-a641-6232c2608928",
        "client_secret": "daemon-app-secret",
        "scope": scope,
    }).encode()
    req = urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=form)
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read())["access_token"]


# --- control plane: a workspace and a lakehouse to write into ---
step("control plane: mint token, create workspace + lakehouse")
fabric_token = entra_token("https://api.fabric.microsoft.com/.default")
ws = post_json(f"{FABRIC}/v1/workspaces", {"displayName": "sailws"}, fabric_token)
post_json(f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", {"displayName": "lake"}, fabric_token)
print(f"workspace: {ws['id']}")

# --- data plane: PySpark over Spark Connect to Sail ---
from pyspark.sql import SparkSession  # noqa: E402

step("connect to sail over Spark Connect")
for attempt in range(30):  # sail may still be starting
    try:
        spark = SparkSession.builder.remote(os.environ["SPARK_REMOTE"]).getOrCreate()
        spark.sql("SELECT 1").collect()
        break
    except Exception:
        if attempt == 29:
            raise
        time.sleep(2)
print("connected to sail")

url = "az://sailws/lake.Lakehouse/Tables/events"

# Rows via SQL VALUES, not createDataFrame: VALUES keeps everything
# server-side, so the engine builds the rows. A local relation would ship
# them from the client and prove less about the engine under test.
#
# This was also a workaround once, for an engine that served
# spark.sql.session.localRelationSizeLimit in a form the client could not
# parse. That no longer applies on the pinned engine — createDataFrame here
# was measured working — so the witness argument is the whole reason now.
step("delta write (overwrite, 3 rows)")
df = spark.sql(
    "SELECT * FROM VALUES (1,'signup','eu'), (2,'purchase','us'), (3,'signup','us')"
    " AS t(id, kind, region)"
)
df.write.format("delta").mode("overwrite").save(url)
print("delta write OK")

step("sql over delta")
back = spark.read.format("delta").load(url)
back.createOrReplaceTempView("events")
rows = spark.sql("SELECT kind, COUNT(*) AS n FROM events GROUP BY kind ORDER BY kind").collect()
got = {r["kind"]: r["n"] for r in rows}
assert got == {"purchase": 1, "signup": 2}, got
print(f"sql over delta OK: {got}")

# Append — a second Delta commit exercises the conditional-PUT log protocol.
step("delta append")
spark.sql("SELECT * FROM VALUES (4,'purchase','eu') AS t(id, kind, region)") \
    .write.format("delta").mode("append").save(url)
n = spark.read.format("delta").load(url).count()
assert n == 4, n
print("delta append OK (4 rows)")

# --- deprecation-audit probes: turn "unverified vs delta-spark" into knowns ---

# Time travel by version. (The SQL `VERSION AS OF` form also works — see
# docs/engine-matrix.md; an earlier comment here called it a Sail gap.)
step("time travel (versionAsOf 0)")
v0 = spark.read.format("delta").option("versionAsOf", 0).load(url).count()
assert v0 == 3, v0
print("time travel OK (versionAsOf 0 -> 3 rows)")

# MERGE INTO (copy-on-write): update one row, insert another. Finding: Sail
# resolves path-based delta.`az://…` for READS but not as a MERGE target —
# the target must be a catalog table, so register the location first.
step("MERGE INTO (copy-on-write, via registered table)")
spark.sql(f"CREATE TABLE events_t USING delta LOCATION '{url}'")
spark.sql("""
    MERGE INTO events_t AS t
    USING (SELECT * FROM VALUES (4,'refund','eu'), (5,'signup','ap') AS s(id, kind, region)) AS s
    ON t.id = s.id
    WHEN MATCHED THEN UPDATE SET t.kind = s.kind
    WHEN NOT MATCHED THEN INSERT *
""")
merged = {r["id"]: r["kind"] for r in spark.read.format("delta").load(url).collect()}
assert merged[4] == "refund" and merged[5] == "signup" and len(merged) == 5, merged
print("MERGE INTO OK (update + insert, via registered table)")

# Fabric-style abfss:// (Hadoop form, the URL shape unmodified production
# notebooks use). Sail parses container@account.dfs.fabric.microsoft.com and
# the endpoint override redirects the requests to the emulator — if this
# holds, no abfss->az shim is needed anywhere.
step("abfss:// (Fabric Hadoop URL form)")
abfss = "abfss://sailws@onelake.dfs.fabric.microsoft.com/lake.Lakehouse/Tables/abfss_probe"
spark.sql("SELECT * FROM VALUES (1,'x') AS t(id, v)") \
    .write.format("delta").mode("overwrite").save(abfss)
assert spark.read.format("delta").load(abfss).count() == 1
print("abfss:// (Fabric Hadoop form) OK — no shim needed")

# --- executable compatibility boundary -------------------------------------

def expect_unavailable(name, operation):
    step(f"compatibility probe: {name}")
    try:
        operation()
    except Exception as error:
        print(f"known gap confirmed: {name} ({type(error).__name__})")
        return
    raise AssertionError(f"Sail capability changed: {name} unexpectedly succeeded; update the parity matrix")


expect_unavailable("SparkContext / RDD", lambda: spark.sparkContext.parallelize([1, 2]).count())
expect_unavailable("Py4J JVM bridge / Java and Scala UDFs", lambda: spark._jvm.java.lang.System.nanoTime())
# Sail stores arbitrary Spark configuration, so this setting appears to work,
# but there is no JVM/classloader that could load the referenced JAR.
spark.conf.set("spark.jars", "/tmp/compat-probe.jar")
assert spark.conf.get("spark.jars") == "/tmp/compat-probe.jar"
print("known divergence confirmed: spark.jars is accepted but inert (no JVM classloader)")


def start_stream():
    query = (spark.readStream.format("rate").load().writeStream
             .format("memory").queryName("sail_stream_probe").start())
    query.awaitTermination(10)


expect_unavailable("Structured Streaming execution", start_stream)
expect_unavailable("OPTIMIZE", lambda: spark.sql("OPTIMIZE events_t").collect())
expect_unavailable("VACUUM", lambda: spark.sql("VACUUM events_t RETAIN 168 HOURS").collect())
step("compatibility probe: change data feed")
# Bare Connect — this suite does not install delta_ops. The Livy agent does,
# and the engine-matrix sail-delta column is the notebook-API witness.
cdf_probe = (spark.read.format("delta").option("readChangeFeed", "true")
             .option("startingVersion", 0).load(url))
cdf_probe.collect()
assert "_change_type" not in cdf_probe.columns, cdf_probe.columns
print("known divergence confirmed: CDF options are accepted but return a normal snapshot (unintercepted Sail)")

# Two overwrite commits from separate sessions, released at one barrier.
#
# WHAT THIS ASSERTS, AND WHY IT IS NOT "exactly one commits". The barrier
# synchronises the START of the two writes, not their overlap. If writer A
# finishes its commit before writer B reads the table version, B legitimately
# commits on top of A: two successes, no conflict, and nothing is wrong.
# Serialisation is a VALID outcome of racing two writers, and an assertion that
# forbids it fails on a correct server — this suite asserted
# `outcomes.count("committed") == 1` and went red on exactly that, 1 run in 13.
#
# So the assertion is the INVARIANT that holds either way: **each successful
# commit produces its own new version in the Delta log, and no two writers ever
# land on the same one.** That is what OneLake's conditional-create (If-None-
# Match) actually guarantees, and it is what a broken guard would violate — two
# writers overwriting one version file, losing an update silently. Counting
# commit files against successful writers catches that; counting rejections
# only catches the scheduler.
#
# This is the repo's own lesson applied where it was still outstanding: when a
# race lives in a window too small to hit reliably, make the interleaving an
# input rather than sampling for it — or, where you cannot, assert the property
# that survives every interleaving instead of the one you hoped to observe.
#
# The retry side of this is Sail's, and it is bounded: sail-delta-lake 0.6.6
# commits under `for attempt_number in 1..=total_retries`
# (crates/sail-delta-lake/src/transaction/mod.rs), and an overwrite is neither a
# creation nor a blind append, so its `effective_max_retries` is 0 — one
# attempt, then `MaxCommitAttempts`. The loser fails in milliseconds. Nothing
# here can spin; what it can do is block, since every call below is a gRPC round
# trip with no deadline, which is what `step()` exists to bound.
conflict_url = "az://sailws/lake.Lakehouse/Tables/concurrent_probe"
step("seed the concurrent-writer table")
spark.range(1).write.format("delta").mode("overwrite").save(conflict_url)
storage_token = entra_token("https://storage.azure.com/.default")


def delta_log_versions(table):
    """Version numbers present in a table's _delta_log, read through OneLake.

    Fetches 00000…N.json until one 404s. The emulator serves the Blob surface
    on the account-prefixed path, the same form the byte-level check below uses.
    """
    found = []
    for version in range(64):
        url = (f"{FABRIC}/onelake/{ws['id']}/lake.Lakehouse/Tables/{table}"
               f"/_delta_log/{version:020d}.json")
        req = urllib.request.Request(url, headers={"Authorization": "Bearer " + storage_token})
        try:
            with urllib.request.urlopen(req, timeout=30):
                found.append(version)
        except urllib.error.HTTPError as err:
            if err.code == 404:
                break
            raise
    return found


step("read _delta_log versions before the race")
before = delta_log_versions("concurrent_probe")
assert before, "the seed commit is missing; the probe below would prove nothing"
barrier = threading.Barrier(2)


def concurrent_overwrite(value):
    session = SparkSession.builder.remote(os.environ["SPARK_REMOTE"]).create()
    try:
        barrier.wait(timeout=30)
        session.range(value, value + 10).write.format("delta").mode("overwrite").save(conflict_url)
        return "committed"
    except Exception as error:
        return f"rejected:{type(error).__name__}"
    finally:
        # Release the session on the SERVER, and deliberately NOT session.stop().
        #
        # This is what hung the job. stop() calls client.close(), whose first act
        # is ExecutePlanResponseReattachableIterator.shutdown() — and that is
        # PROCESS-WIDE, not per session. It takes a CLASS-level lock, hands the
        # CLASS-level release thread pool to ThreadPoolExecutor.shutdown()
        # (wait=True by default), and holds the lock until the pool drains. Every
        # new query in this process builds one of those iterators, and its
        # __init__ takes the same class lock — so one worker's stop() blocks the
        # MAIN session's next query for as long as the drain takes.
        #
        # What is draining is outstanding ReleaseExecute RPCs under the Connect
        # retry policy, whose own comment says the retry count is chosen "so that
        # the maximum tolerated wait is guaranteed to be at least 10 minutes"
        # (pyspark/sql/connect/client/retries.py, DefaultPolicy). A losing writer
        # is exactly the case that leaves one outstanding: its execution ends in
        # an error, which submits _release_all right as the session is torn down.
        # That is the intermittency, and the CI log is its shape — both sessions
        # released, then 24 minutes of nothing, because no request ever reached
        # Sail again.
        #
        # release_session() sends the same ReleaseSession RPC (so Sail still logs
        # the removal) and touches no shared state. It can still retry on its own
        # thread; the step watchdog above bounds that. Best-effort like the
        # stop() it replaces — this is teardown, and a failure here must not be
        # mistaken for the race's verdict — but said out loud, not swallowed.
        try:
            session.client.release_session()
        except Exception as teardown_error:  # noqa: BLE001
            print(f"note: releasing the writer session failed: {teardown_error!r}",
                  flush=True)


step("race two overwrite writers on one Delta version")
with ThreadPoolExecutor(max_workers=2) as executor:
    outcomes = list(executor.map(concurrent_overwrite, (100, 200)))
step("read _delta_log versions after the race")
committed = outcomes.count("committed")
rejected = sum(outcome.startswith("rejected:") for outcome in outcomes)
after = delta_log_versions("concurrent_probe")
new_versions = [v for v in after if v not in before]

# At least one writer must get through, or the probe proves nothing about
# conflict handling — it would just be two failures.
assert committed >= 1, outcomes
assert committed + rejected == 2, outcomes
# THE INVARIANT: one new version per successful commit. Fewer means two writers
# landed on the same version and one update was lost — precisely the failure
# the conditional PUT exists to prevent. More would mean a commit nobody made.
assert len(new_versions) == committed, (outcomes, before, after)
# Contiguous, so a commit did not skip a version and leave a hole a reader
# would stop at.
assert after == list(range(len(after))), after
# The surviving table is one writer's full overwrite, not a blend of both.
step("read back the table the race left behind")
rows = spark.read.format("delta").load(conflict_url).collect()
assert len(rows) == 10, rows
assert {row[0] for row in rows} in ({v for v in range(100, 110)}, {v for v in range(200, 210)}), rows
if rejected:
    print(f"concurrent overwrite: conflict rejected as expected {outcomes}, "
          f"+{len(new_versions)} version(s)")
else:
    print(f"concurrent overwrite: serialised (both committed, versions {new_versions}) — "
          "a valid interleaving; the invariant checked is one version per commit")

# The engine's writes are real bytes in OneLake: read the first Delta commit
# back through the Blob surface ourselves (same account-prefixed path form).
step("read the delta log through the Blob surface")
req = urllib.request.Request(
    f"{FABRIC}/onelake/{ws['id']}/lake.Lakehouse/Tables/events/_delta_log/"
    "00000000000000000000.json",
    headers={"Authorization": "Bearer " + storage_token},
)
with urllib.request.urlopen(req, timeout=30) as r:
    first_commit = r.read().decode()
assert '"protocol"' in first_commit and '"metaData"' in first_commit, first_commit[:200]
print("delta log readable through the Blob surface (protocol + metaData present)")

# The conditional-create contract itself, with the interleaving as an INPUT
# rather than sampled for.
#
# The racing writers above can only observe a rejection when they happen to
# collide, and they legitimately may not (see that block). This asserts the
# same guarantee with no race at all: version 0 of the events table exists, so
# a put-if-absent against that exact path must be refused. It is the rejection
# half of the concurrency story, held to a deterministic standard, and it is
# the 409 the racing writers see when they DO collide.
#
# TestConcurrentDeltaCommitRace pins the same mechanism at unit level; this
# pins it end-to-end, through the Blob surface a real client reaches, with the
# token a real client mints.
step("put-if-absent against an existing commit (expect 409)")
conflict_probe = urllib.request.Request(
    f"{FABRIC}/onelake/{ws['id']}/lake.Lakehouse/Tables/events/_delta_log/"
    "00000000000000000000.json",
    method="PUT",
    data=b'{"commitInfo":{"operation":"SHOULD NOT LAND"}}',
    headers={"Authorization": "Bearer " + storage_token, "If-None-Match": "*"},
)
try:
    urllib.request.urlopen(conflict_probe, timeout=30)
    raise AssertionError("put-if-absent overwrote an existing Delta commit")
except urllib.error.HTTPError as refused:
    assert refused.code == 409, refused.code
print("put-if-absent on an existing commit refused with 409")

# And it refused WITHOUT damaging what was there. A 409 that still replaced the
# bytes would satisfy the status assertion and lose the commit anyway.
with urllib.request.urlopen(req, timeout=30) as r:
    assert r.read().decode() == first_commit, "the refused PUT modified the commit"
print("the refused PUT left the existing commit byte-identical")

print("PASS: sail e2e")
