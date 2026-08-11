# 50 — What real Fabric code actually does with `sc` and `spark._jvm`

Design input for closing the RDD/`_jvm` gap on the Sail engine, captured by
measuring rather than by reading the RDD API. The question this answers is not
"what does `SparkContext` contain" — it is **"which of it does Fabric code in
the wild actually call"**, because a facade shaped by the first question is a
reimplementation of Spark, and one shaped by the second is an afternoon of
work.

The result is sharper than expected: **across every corpus checked, the entire
measured `sc`/`_jvm` surface is logging, plus one four-element smoke idiom.**

## The problem being designed for

[20-lakesail-engine.md](20-lakesail-engine.md) grades `sc` / RDD / `spark._jvm`
❌ on the default engine. The gap decomposes into three problems with
different physics, and conflating them is what makes it look insurmountable:

1. **Protocol-impossible.** `sc`, the RDD API and `spark._jvm` do not exist in
   Spark Connect for *any* engine — Apache's own JVM Connect server has the
   same hole. No Sail release fixes this; a fix lives client-side, in the
   agent's namespace, where `sc` is already bound (today, to a guide-rail
   stub).
2. **Engine-missing.** Structured streaming and `OPTIMIZE`/`VACUUM` are absent
   in Sail v0.6.6 but are engine features Sail can grow (the emulator already
   routes `OPTIMIZE`/`VACUUM` through delta-rs). Time fixes these.
3. **JVM-essential.** Java/Scala UDFs and `spark.jars` mean executing JVM
   bytecode. No Rust engine will ever do this without hosting a JVM — which
   the opt-in overlay (`docker-compose.spark-jvm.yml`) already does.

Only problem 1 needs new design, and this capture bounds it.

## Method

Four corpora, most consumer-shaped first:

| Corpus | What it represents | How searched |
|---|---|---|
| `examples/*/definitions/**/notebook-content.py` + `e2e` fixtures (this repo) | notebooks we ship and tell people to copy | `grep` over the tree |
| `contoso-data-platform` (`platform/`, `sources/`, `gold/`) | a real consumer platform | `grep` over the tree |
| `microsoft/fabric-samples` | Microsoft's own consumer-shaped samples | GitHub code search |
| `MicrosoftDocs/fabric-docs` | what Microsoft teaches | GitHub code search |

Patterns: `sc.`, `sparkContext`, `_jvm`, `parallelize`, `textFile`,
`mapPartitions`, `broadcast(`, `accumulator`, `readStream`, `writeStream`,
Java/Scala UDF registration, `spark.jars`.

**Every remote hit was verified by fetching the file, not trusted from the
count.** That step mattered: the `spark._jvm` query returned pages that could
have been tokenizer artifacts (matching `spark` and `jvm` separately), and
fetching showed they were real — all carrying the same log4j idiom.

*Limits, stated rather than implied:* GitHub code search indexes the default
branch only, skips very large files, and is historically weak on `.ipynb` —
which is exactly where notebook code lives. All remote counts are therefore
**lower bounds**. The local corpora have no such blind spot and were swept
exhaustively.

## Findings

| Pattern | This repo (shipped) | cdp | fabric-samples | fabric-docs | The actual idiom |
|---|---|---|---|---|---|
| `sparkContext.setLogLevel` | 0 (harness only) | 0 | **2** — both `spark_context.setLogLevel(...)` | troubleshooting pages | logging control |
| `sc.parallelize` | 0 | 0 | 0 | **4** — all `sc.parallelize(Seq(1,2,3,4)).toDF().count()` in the diagnostic-emitter pages | a smoke test to generate task metrics |
| `spark._jvm` | 0 | 0 | 0 | **3 verified** — all `spark._jvm.org.apache.log4j` → `LogManager.getLogger(...)` | logging, again |
| `readStream`/`writeStream` | 0 | 0 | 0 | **11 pages** — real feature docs (real-time mode, triggers, output modes) | structured streaming proper |
| `textFile`, `mapPartitions`, `broadcast`, `accumulator`, Java/Scala UDFs, `spark.jars` | 0 | 0 | 0 | 0 | absent everywhere |

Three observations the table compresses:

- **The measured `sc`/`_jvm` surface is logging.** `setLogLevel`, and a log4j
  logger reached through `_jvm`. Both are side effects the agent can provide
  *genuinely* — routing to Python logging is a real implementation of the
  measured contract, not a fabricated success.
- **`sc.parallelize` appears in Microsoft's corpus only as a smoke idiom** —
  four elements, `.toDF()`, `.count()` — used to make the diagnostic emitters
  emit something. Nobody in any checked corpus computes with RDDs.
- **Structured streaming is the one substantive gap with real documented
  usage**, and it is problem 2 (engine roadmap) and problem 3 (the JVM overlay
  runs it today), not facade material.

## What this licenses

**Tier A — the facade, sized by the table.** Replace the `sc` guide-rail stub
with a facade implementing exactly: `setLogLevel` (forwarded to the session),
the `parallelize(seq)` → DataFrame chain far enough to carry the smoke idiom
(`.toDF()`, `.count()`, and the `.map().sum()` shape the JVM oracle already
verifies), and a `_jvm.org.apache.log4j` path answering `LogManager.getLogger`
with a Python-logging-backed logger. **Everything else keeps today's loud
refusal**, so the facade's edge stays visible instead of approximate.

Direction of failure: real Fabric has the full API, so a correct subset fails
safe — code works there, and here either works identically or raises the
existing pointer. The differential oracle exists in-repo: `e2e/spark-jvm` runs
the same expressions on classic JVM `sc`, so every facade method gets a
compared result, not an asserted one.

**Tier B — streaming stays where it is**: on Sail's roadmap and on the JVM
overlay, which is one compose flag and is CI-verified weekly. A facade cannot
fake a streaming engine and should not try.

**Explicitly NOT licensed:** `textFile`, partition-level APIs, accumulators,
broadcasts, Java/Scala UDFs, `spark.jars` emulation. Zero measured uses;
implementing them would be shaped by the API rather than by evidence, and every
line would be fabrication surface.

## Not claimed

This capture is a lower bound on the world's RDD usage, not a census — private
enterprise notebooks are exactly the code no search reaches. The claim is
narrower: **among everything Microsoft publishes and everything this family
ships, no `sc`/`_jvm` use exists that the Tier A facade would not cover.** If
a consumer arrives with `mapPartitions`, the refusal message already tells
them about the JVM overlay; that is the correct answer until someone measures
a corpus that says otherwise.
