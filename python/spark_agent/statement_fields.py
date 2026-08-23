"""What `/statements` accepts, refuses, and merely notes.

Split out of agent.py for the reason catalog.py and usercontext.py are: agent.py
calls getOrCreate() at import, so nothing defined there can be unit tested. A
rule about which requests are rejected is the last thing that should live where
its tests cannot reach it.
"""
import sys

# THE ROUTE USED TO ACCEPT `env` AND `spark_conf` AND APPLY NEITHER (#349). A
# caller got `{"status":"ok"}` and its field on the floor, which is the kind of
# silence that sends someone looking in the wrong place: it already produced a
# bug filed against databricks-emulator rather than against this agent.
#
# NAMED IN THE LOG RATHER THAN REFUSED, and that is a correction. The first cut
# of this refused them outright, on the issue's premise that "nothing sends
# them any more" -- true of databricks-emulator's MAIN, and false of every
# databricks-emulator anyone can run. The fix that stopped sending them
# (databricks-emulator#75) merged 2026-08-21; the newest release, v0.2.9, is
# from 2026-08-20. So no published image omits them, and `e2e/databricks-chain`
# -- which pins 0.2.4 -- failed on the refusal within minutes.
#
# That is the same version-skew hazard this file already reasons about for
# UNKNOWN fields, and it applies just as much to fields we know are inert: the
# emulator and its callers are separate images with independent versions, so a
# refusal can only be added once every caller that sends the field is gone from
# the fleet.
#
# TO PROMOTE THESE TO REFUSALS: wait for a databricks-emulator release carrying
# #75, move the pins that consume it (e2e/databricks-chain and any platform
# repo), then move these entries into a REFUSED map and return an error
# envelope instead of logging. The reasons below are why they will never be
# implemented; the log is only how they are reported until then.
#
# NEITHER WILL BE IMPLEMENTED, for two different reasons.
#
# `env` could be implemented -- `task_scope` already gives each session its own
# environment -- but Fabric's Livy statement payload is `{"code", "kind"}`
# (get-started-high-concurrency-livy.md). Adding it would grow a surface the
# product does not have, in an emulator whose whole value is matching the
# product. The way to set a task's environment is the way databricks-emulator
# now does it: in the generated code, where no agent can drop it.
#
# `spark_conf` should not be implemented at all. A statement-level
# `spark.conf.set` is not statement-scoped -- it lands on the session's Spark
# session and outlives the statement, and where `catalog.isolate` could not give
# the Livy session its own SparkSession it lands on the SHARED one and leaks
# into every other session. `apply_environment` gets away with it only because
# an Environment binds once per agent and refuses a second.
IGNORED_STATEMENT_FIELDS = {
    "env": "set a task's environment in the code it runs, where no agent can "
           "drop it; Fabric's Livy statement payload is {code, kind}",
    "spark_conf": "a statement-level conf is not statement-scoped: it outlives "
                  "the statement on the session's Spark session, and on a "
                  "shared session it leaks into every other one",
}

# Everything this route reads, each name traceable to its reader: the dispatch
# below, `remember_context`, `_apply_onelake_security`, and the register call.
#
# UNKNOWN NAMES ARE LOGGED, NOT REFUSED, and the difference matters here. This
# is an INTERNAL protocol between two images that version independently, so a
# newer emulator sending a field an older agent has not heard of must degrade,
# not fail. Refusing would turn a rolling upgrade into an outage; saying so in
# the log removes the silence without that risk.
KNOWN_STATEMENT_FIELDS = {
    "code", "kind", "session", "jobId", "cellIndex",
    "principal", "workspace", "item", "lakehouse",
    "schema", "schemas", "tables",
    "workspaceId", "lakehouseId", "notebookId",
    "currentWorkspaceId", "defaultLakehouseId", "currentNotebookId",
    "currentJobId", "isForPipeline",
}


def check(req):
    """Say what this route is dropping. Always returns None -- it never refuses.

    The harm #349 documents is the SILENCE: a field accepted and discarded,
    which sent a reader to file a bug against the wrong repository. Saying so
    fixes that. Refusing would fix it harder and break every caller running a
    released databricks-emulator, which all of them are.
    """
    for name, why in IGNORED_STATEMENT_FIELDS.items():
        if name in req:
            print(f"agent: /statements does NOT apply {name!r} and is dropping "
                  f"it: {why}", file=sys.stderr, flush=True)
    unknown = sorted(set(req) - KNOWN_STATEMENT_FIELDS
                     - set(IGNORED_STATEMENT_FIELDS))
    if unknown:
        print(f"agent: /statements ignoring unknown field(s): {', '.join(unknown)}",
              file=sys.stderr, flush=True)
    return None


