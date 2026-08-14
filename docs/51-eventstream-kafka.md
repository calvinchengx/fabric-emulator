# 51 — Eventstream exec: a real Kafka broker behind Fabric Spark options

**Status: shipped, opt-in broker, both engines (🟠).** Eventstream item
management always works. Execution — a real Kafka topic the Fabric notebook
API can subscribe to — is real when an Apache Kafka KRaft broker is attached.
No broker → honest 501. Sail is the default engine; the JVM overlay keeps a
native `spark-sql-kafka` source. Neither path maps Eventstream onto `rate`.

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
   JSON `key`/`value` bytes into that topic. Not thirty connectors.
4. **Spark adapter, both engines** — `python/spark_agent/eventstream_kafka.py`:
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

Both create an Eventstream item, produce JSON records through the Custom HTTP
source, run `format("kafka")` + eventstream options + `foreachBatch`, and
assert the Kafka schema (`key`, `value`, `topic`, `partition`, `offset`) and
row count. A wrong item id must fail. `rate` must not appear.

## Boundaries (deliberate, not backlog)

- **Eventhouse / Kusto streaming destination** stays 🔴. kustainer has no
  streaming ingestion ([25-rti-kusto.md](25-rti-kusto.md)).
- Lakehouse / Reflex destinations and operators (Filter, GroupBy, windows)
  are not this slice.
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
