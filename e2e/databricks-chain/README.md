# The family chain: fabric → databricks-emulator → Spark agent → Sail

fabric-emulator submits a Databricks activity to **databricks-emulator**, which
runs it on the family's Spark agent, which runs it on Sail. Databricks' own
**`databricks-sdk`** then reads the workspace side and confirms the run.

```sh
python3 e2e/databricks-chain/run.py
```

## Why this exists, and it is not mainly the witness

`FABRIC_DATABRICKS_URL` was documented in the README, in
[docs/04-configuration.md](../../docs/04-configuration.md) — which names
databricks-emulator as the intended target — in the v0.25.0 release notes, and
in `internal/api/databricksremote.go`. It appeared in **no workflow anywhere**.

The parity row grades the Databricks activities
**🟢 Real (notebook + python, local or `FABRIC_DATABRICKS_URL`)**. The local
half ran on every push. The remote half had never executed. So this is an
unexercised code path getting exercised; the third-party witness is the side
effect, not the point.

## Why the SDK rather than a REST call

`databricks-sdk` parses the workspace's answers into its own dataclasses. If
fabric's submission produced a job recorded in a shape Databricks does not use,
**the SDK itself fails to read it back** — which a hand-written `requests.get`
asserting the fields we happen to send cannot do. That is the difference between
a client that can disagree with us and one that cannot, and it is why this sits
in a stronger evidence tier than a generic `az rest` call.

## What is asserted, and why both ends

| end | assertion |
|---|---|
| Fabric | the activity reaches `Completed` **and its output names a Databricks executor** |
| Databricks | the SDK finds a job, a run, and a run **referencing `etl.py`** specifically |

Either alone is satisfiable by a lie. An activity that quietly fell back to the
**local** Spark agent would report `Completed` and look identical from the
Fabric side — ruling that out is the whole reason the chain exists. And a run
that merely exists proves nothing about whether it is ours.

`DATABRICKS_SPARK_CONNECT_URL` is set deliberately: without it,
databricks-emulator's `run-now` fails naming the missing engine rather than
pretending to succeed, which is the behaviour that makes this chain worth
wiring rather than faking.

## Two phases, forced by the environment

`run.py` brings the stack up in two passes. databricks-emulator **mints** its
admin PAT on first start and writes it to its data dir; it does not take one
from the environment. fabric needs that PAT at startup. Both emulators are
**distroless**, so there is no shell inside the compose to wait-and-exec with.
So: start the databricks side, read the PAT from the bind-mounted data dir,
then start fabric and the client with it.

## The trap this suite already fell into

The first run failed with `no file at "Files/jobs/etl.py"`, which reads like a
broken feature and was a broken fixture: the seed writes used the **Fabric**
bearer, and OneLake requires the **Storage** audience and rejects the Fabric one
outright (`internal/onelake/onelake_test.go` pins that rejection).

The fix is two lines. The lesson is the third: the driver now **reads the file
back immediately after seeding** and fails there if it is absent. A suite that
cannot tell "the fixture never landed" from "the feature is broken" wastes the
run it fails on.
