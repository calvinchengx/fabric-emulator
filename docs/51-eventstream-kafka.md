# 51 — Eventstream exec: a real Kafka broker behind Fabric Spark options

**Status: shipped, opt-in sidecar, graded Real.** Eventstream item
management always works. Execution — a real Kafka topic the Fabric notebook
API can subscribe to — is real when an Apache Kafka KRaft broker is attached.
No broker → honest 501. `--profile eventstream` stays opt-in. Sail is the
default engine; the JVM overlay keeps a native `spark-sql-kafka` source.
Neither path maps Eventstream onto `rate`. Destinations and operators sit
on the Custom HTTP produce path: **Lakehouse** (Delta append), **Reflex**
(item job), **Eventhouse** (Kusto direct ingest), and **Filter / GroupBy /
tumbling Window** on the produce batch. Kafka stays the raw source; dests
see operator output.

This is the same move as Eventhouse ([25-rti-kusto.md](25-rti-kusto.md)):
terminate the Fabric names ourselves, relay bytes to a real engine.

## Why Apache Kafka, not Redpanda

The family rule is no BSL surprise. Redpanda was Apache-2.0 and then moved to
the Business Source License; pinning it would put a license change under an
opt-in profile the way an unpinned `latest` tag puts a binary change under a
witness. **`apache/kafka` is ASF, Apache License 2.0, KRaft (no ZooKeeper),
and multi-arch** (`linux/amd64` + `linux/arm64`), so it runs natively on
Apple silicon — unlike kustainer, which needs AVX2.

The image is digest-pinned in `docker-compose.yml`. Refresh with:

```bash
docker buildx imagetools inspect apache/kafka:3.9.1
```

## The two Fabric surfaces (one product)

Fabric Eventstream is not "the item **or** the notebook API". Both exist:

- **Item:** `POST /v1/workspaces/{ws}/eventstreams` — sources, streams,
  operators, destinations. This slice mints a DefaultStream `datasourceId`
  and a Kafka topic `{itemId}.{datasourceId}`.
- **Notebook** ([notebook-with-event-stream](https://learn.microsoft.com/en-us/fabric/data-engineering/notebook-with-event-stream)):

```python
df_raw = spark.readStream.format("kafka").options(**{
    "eventstream.itemid": item_id,
    "eventstream.datasourceid": datasource_id,
}).load()
# schema: key, value, topic, partition, offset, timestamp
df_raw.writeStream.foreachBatch(showDf).outputMode("append").start()
```

Microsoft's adapter uses the notebook Entra token to resolve those IDs to a
Kafka endpoint. User code has **no** `kafka.bootstrap.servers`. It is **not**
`rate` (`timestamp` / `value`). Mapping Eventstream options onto `rate` would
paint the engine matrix green with the wrong schema; that stays forbidden.

## What the emulator does

```text
notebook  --format kafka + eventstream.*-->  spark-agent
spark-agent  --GET /v1/eventstreams/{item}/sources/{ds}-->  fabric-emulator
  JVM:  rewrite to kafka.bootstrap.servers + subscribe → OSS kafka source
  Sail: GET …/sources/{ds}/events → LocalRelation (Kafka columns)
        foreachBatch runs in the agent (Sail cannot pickle UDFs)
notebook  --format kafka + bootstrap/subscribe/pattern/assign-->  spark-agent
  JVM:  spark-sql-kafka jar
  Sail: kafka-python consume → createDataFrame (bytes on Sail)
notebook  --write/writeStream format kafka-->  spark-agent
  JVM:  spark-sql-kafka jar
  Sail: kafka-python produce from collected rows
producer / Custom HTTP  -->  Kafka
POST …/eventstreams  -->  mint datasourceId + CreateTopics
```

1. **Broker sidecar** — `apache/kafka:3.9.1`, `--profile eventstream`.
2. **Item create provisions a stream** — DefaultStream id, topic name stored
   on the item, `CreateTopics` against the broker when one is attached.
   Unknown IDs fail at resolve (404), not as an empty stream.
3. **Custom source** — `POST …/eventstreams/{id}/sources/{ds}/events` writes
   JSON `key`/`value` bytes into that topic. Not thirty connectors. When a
   destination is bound, those same bytes (or the operator output) are
   drained after the produce succeeds.
4. **Lakehouse destination** — emulator-native
   `POST …/eventstreams/{id}/destinations` with `{type, itemId, table}`.
   Fabric's topology JSON has no public REST (same as Reflex triggers).
   `type` is `Lakehouse`, `Reflex`, or `Eventhouse`.
5. **Reflex destination** — same bind surface, `{type: Reflex, itemId}`.
   A trigger on that Reflex with
   `eventType: Microsoft.Fabric.Eventstream.EventReceived` and
   `source.itemId` the Eventstream starts the action job per produced
   event (`invokeType: EventTriggered`, `TriggerEvent.Key` / `.Value`).
6. **Eventhouse destination** — `{type: Eventhouse, itemId, table}` and
   optional `database`. After produce, operator output is ingested with
   `.create-merge` + `.ingest inline` into the isolated engine database
   for that KQL Database — the path kustainer actually supports. This is
   **not** Fabric's streaming-ingest protocol and not `Kusto.Ingest`.
   No engine attached → produce 502 naming `--kql-url`.
7. **Operators** — emulator-native `POST …/eventstreams/{id}/operators`.
   Filter (`eq`/`ne`/`gt`/`gte`/`lt`/`lte`/`contains`/`exists`), GroupBy
   (`count`/`sum`/`min`/`max`/`avg`), and tumbling Window (this batch
   only; stamps `_window_start`). Join / Union / Expand and
   hopping/sliding are refused by name. Kafka consume is still unfiltered.
8. **Spark adapter, both engines** — `python/spark_agent/eventstream_kafka.py`:
   - **JVM:** wraps classic `readStream.format("kafka").load()` when both
     eventstream options are set: Entra-gated lookup, then
     `kafka.bootstrap.servers` + `subscribe` on the real OSS Kafka source
     (`spark-sql-kafka-0-10_2.12` matching Spark 3.5.5).
   - **Sail:** the engine has no Kafka source and rejects `foreachBatch` at
     `start()`. The wrap consumes through
     `GET /v1/eventstreams/{item}/sources/{ds}/events`, materialises a
     LocalRelation with the Kafka schema, and runs `foreachBatch` in the
     agent. One micro-batch, announced on stderr, no checkpoint — the same
     class of wrap as CDF / JSON `multiLine`. Native `format("kafka")` with
     `kafka.bootstrap.servers` plus `subscribe` / `subscribePattern` /
     `assign` is the same wrap without the Fabric IDs: driver consume →
     Kafka-schema LocalRelation on Sail. JSON offsets, `includeHeaders`,
     SASL PLAIN, GSSAPI, PEM SSL, and JKS/P12 truststores are honoured; a
     kafka *sink* produces from the driver. JVM native kafka still uses the
     jar.

GET the item to read `properties.streams[0].id` — that is the
`eventstream.datasourceid` the notebook snippet needs.

## Running it

Default engine (Sail behind Livy):

```bash
docker compose --profile eventstream \
  -f docker-compose.yml -f docker-compose.override.yml \
  -f docker-compose.eventstream.yml up
```

Or `make up-eventstream`. Two opt-ins, same grammar as RTI: the profile
starts the broker; the overlay sets `FABRIC_KAFKA_BOOTSTRAP`. Without the
overlay, create still returns an item and Spark resolution 501s.

Native Kafka source (checkpointed streaming, no LocalRelation) is the JVM
overlay: add `-f docker-compose.spark-jvm.yml` to that command, or
`make up-jvm` plus the eventstream profile and overlay.

## The witness

`e2e/eventstream` runs the same notebook snippet on both engines:

- **JVM** — CI job **`eventstream`** in `.github/workflows/spark-jvm.yml`
  (weekly/manual). Real OSS kafka source + real `foreachBatch`.
- **Sail** — CI job **`eventstream-sail`** in `.github/workflows/ci.yml`.
  Emulator consume + LocalRelation + local `foreachBatch`.

Both create an Eventstream item, bind a Lakehouse destination, produce JSON
records through the Custom HTTP source, assert the destination
`Tables/<name>/_delta_log` commit (row count and field names), then run
`format("kafka")` + eventstream options + `foreachBatch` and assert the Kafka
schema (`key`, `value`, `topic`, `partition`, `offset`) and row count. A
wrong item id must fail. `rate` must not appear.

Eventhouse destination and operators are **Go-unit witnessed** against the
in-process Kusto stand-in and Lakehouse dest tables. They are not in
`e2e/eventstream` — that job does not start `--profile rti`.

The one thing a stand-in cannot settle is whether the emitted KQL parses, and
column names are where that bites: a source field called `kind` is a KQL
keyword, and bare keyword column names earn a `SYN0002` from real Kusto. The
emitter quotes every column (`['kind']`), and **`e2e/rti` witnesses the emitted
form against kustainer** — the keyword list refused bare, the quoted form
created, ingested and queried back ([25-rti-kusto.md](25-rti-kusto.md)).

## Boundaries (deliberate, not backlog)

- **Lakehouse destination** is this slice: Custom HTTP produce → Delta
  append into `Tables/<name>`. Bind is emulator-native REST. Spark-native
  `format("kafka")` writes are not drained; there is no background consumer.
- **Reflex destination** is this slice: Custom HTTP produce → real item
  job via the existing Activator fire path. A `FileCreated` trigger on the
  same Reflex does not fire. No dest bound → no job, even if a stream
  trigger exists.
- **Eventhouse destination** is this slice: Custom HTTP produce → Kusto
  **direct** ingest (`.create-merge` + `.ingest inline`). Same produce
  trigger as Lakehouse; no background consumer. Fabric streaming ingest
  and queued `Kusto.Ingest` stay refused — the engine does not host them
  ([25-rti-kusto.md](25-rti-kusto.md)).
- **Operators** are this slice: Filter, GroupBy, tumbling Window on the
  produce batch. Destinations see the operator output; Kafka /
  DefaultStream stays the raw source. Join / Union / Expand and
  hopping/sliding windows stay refused — they need more than one stream
  or cross-batch state.
- **Sail `format("kafka")`** is a driver consume/produce into a
  Kafka-schema LocalRelation (bytes on Sail). `subscribe` /
  `subscribePattern` / `assign`, JSON `startingOffsets`/`endingOffsets`,
  `includeHeaders`, SASL PLAIN, GSSAPI (JAAS `keyTab` → `KRB5_CLIENT_KTNAME`),
  PEM SSL, and JKS/P12 truststores (converted to PEM for kafka-python) are
  honoured. A kafka *sink* (`write`/`writeStream.format("kafka")`) produces
  from the driver. Checkpointed streaming (`isStreaming`, resume from
  checkpoint) stays on the JVM overlay.
- On Sail the result is materialised (`.explain()` is a LocalRelation; one
  micro-batch; `isStreaming` is false). Checkpointed streaming is the JVM
  overlay.
- Real-Time Hub explorer UX is not this slice.
