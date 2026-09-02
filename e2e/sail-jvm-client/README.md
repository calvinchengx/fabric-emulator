# A JVM Spark Connect client against Sail

`DatabricksSparkJar` runs on the JVM overlay only, because the default engine
(Sail) ships no `spark-submit`. But Sail is a Spark Connect **server**, and
Apache publishes a JVM Connect **client** that needs no cluster of its own. So a
jar's `mainClassName` can run on a real JVM while Sail does the computing, which
is the same move the Livy path makes: terminate the protocol locally rather than
proxy to something we do not have.

This probe is the evidence for that, and the alarm for the day it changes.

```
uv run --frozen --group sail python e2e/sail-jvm-client/run.py
```

It needs a JDK 17+ on PATH and fetches ~81 MB of jars from Maven Central on the
first run (`scripts/fetch_connect_client_jars.py`, cached in `jars/`, gitignored).
No Docker: `run.py` starts Sail in-process, the same way
`scripts/probe_sail_merge_premise.py` does.

## What it measured

Spark **4.1.2** Scala client against Sail **0.7.1**, 2026-09-02. Stable 4.1.2
rather than 4.2.0 because Maven Central publishes 4.2.0 only as previews, and
because pyproject already records that Sail 0.7.0 tolerates a client one minor
behind.

| Works | Refuses |
| --- | --- |
| handshake, `spark.sql` | typed `Dataset.map` (JVM closure) |
| untyped DataFrame ops | `spark.sparkContext()` |
| parquet write then read back | Java/Scala UDFs |
| SQL DDL and temp views | `addArtifact` (no jar upload channel) |
| `args[]` and the exit code | |

The four refusals all need a JVM on the **server** side, and Sail is Rust. They
are asserted to refuse, so a pass fails this run with `THE BOUNDARY MOVED`. That
is the whole point: the MERGE intercept's premise expired quietly for an entire
release because nothing re-asked the question, and `delta_ops.py` kept rewriting
statements Sail could by then plan on its own.

Refusal **wording** is recorded but never enforced, because the design depends on
the outcome and not the phrasing. It is recorded because two of the four are
unreadable — `wildcard with plan ID` and a bare gRPC `INTERNAL: handle add
artifacts` — which is the argument for pre-scanning a submitted jar rather than
letting a user meet those. If Sail ever answers clearly, part of that scan can go.

## What it does not settle

It runs against a bare local Sail writing parquet to a temp directory. The
emulator's real path is OneLake over `abfss://` inside the compose stack, and
[docs/20](../../docs/20-lakesail-engine.md) records the storage URL as exactly
what has separated Sail behaviours before, MERGE included. Green here is
necessary for the runner and nowhere near sufficient.
