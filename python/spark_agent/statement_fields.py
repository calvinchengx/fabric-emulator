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
# REFUSED RATHER THAN IMPLEMENTED, for two different reasons.
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
REFUSED_STATEMENT_FIELDS = {
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
    """Refuse a field this route would silently ignore; name the rest.

    Returns an error envelope, or None to proceed.
    """
    for name, why in REFUSED_STATEMENT_FIELDS.items():
        if name in req:
            return {"status": "error", "ename": "UnsupportedField",
                    "evalue": f"/statements does not apply {name!r}: {why}",
                    "traceback": [f"the request carried {name!r}, which this "
                                  "route would have ignored; refusing rather "
                                  "than accepting it silently"]}
    unknown = sorted(set(req) - KNOWN_STATEMENT_FIELDS
                     - set(REFUSED_STATEMENT_FIELDS))
    if unknown:
        print(f"agent: /statements ignoring unknown field(s): {', '.join(unknown)}",
              file=sys.stderr, flush=True)
    return None


