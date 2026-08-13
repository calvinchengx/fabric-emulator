# The Spark statement-executor agent

The engine-side half of the emulator's Livy and `RunNotebook` support. The Go
layer terminates the REST contract; this holds the SparkSession and executes
the code.

## Why it lives here and not in `e2e/`

It used to live in `e2e/livy/`, and that was a packaging bug rather than a
filing preference. `ghcr.io/calvinchengx/fabric-emulator-spark-agent` is built
from `docker/spark-agent/Dockerfile`, which copies `python/` — so anything
under `e2e/` was never in the published image. Every compose file in this
repository bind-mounted the source tree over the top, which meant the gap was
invisible from inside the project and total from outside it:

```
docker run --rm --entrypoint sh \
  ghcr.io/calvinchengx/fabric-emulator-spark-agent:0.14.0 -c "ls /livy"
  → no such directory
```

A consumer working from published artifacts — no clone — could start the agent
and get a container with no agent in it. Notebook jobs then never reach a
terminal state, which since 0.14.0 surfaces as `NotebookError` on timeout.

Under `python/` the image carries the code by construction, and
`test_the_spark_agent_is_baked_into_its_image` fails if that stops being true.

## Files

| | |
|---|---|
| `agent.py` | the HTTP server and REPL — `/health`, `/statements`, `/close`; refreshes the Files mount around each statement |
| `files_mount.py` | `/lakehouse/default/Files` — pull at bind, write-back + pull at every statement, refuse a second lakehouse |
| `delta_ops.py` | `OPTIMIZE` / `VACUUM` / CDF, routed through delta-rs |
| `storage.py` | the env → `object_store` credential mapping delta-rs needs |

`e2e/livy/` keeps what is genuinely test scaffolding: `client.py`, `run.py` and
the harness compose file.
