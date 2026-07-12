# e2e: dbt-fabricspark

Microsoft's real [dbt-fabricspark](https://github.com/microsoft/dbt-fabricspark)
adapter runs a dbt project against **fabric-emulator's Livy High-Concurrency
surface**, with real Spark computing the models. It is a second, independent
client over the Livy HC layer — a strong parity witness that the emulator speaks
the contract a real Fabric tool expects, not just our own tests.

```
dbt-fabricspark → fabric-emulator (Fabric REST + Livy HC, native) → spark-agent (real Spark SQL)
                       ↑ Entra FedAuth token minted by entra-emulator
```

## Run

```
python e2e/dbt-fabricspark/run.py
```

Brings the stack up with docker-compose and asserts `dbt debug → seed → run →
test` all pass (`--exit-code-from dbt`). JVM Spark makes this a heavier weight
class, like the other Spark e2es (Linux-friendly).

## How it maps onto the emulator

- **Auth.** dbt-fabricspark's own `authentication: int_tests` mode takes a
  pre-minted `accessToken`, so we skip the interactive/MSAL token dance while
  still exercising the emulator's token validation and RBAC end to end. The
  token is forged from entra-emulator with audience
  `https://analysis.windows.net/powerbi/api` — in the emulator's
  `ControlPlaneAudiences` and exactly what dbt's scope
  `…/powerbi/api/.default` resolves to, so one token serves both the Fabric REST
  and Livy legs.
- **High-Concurrency (default).** dbt defaults to `high_concurrency: true`, so it
  drives `POST …/highConcurrencySessions`, polls `GET …/{id}` until `Idle`, and
  runs statements at `…/repls/{replId}/statements` — the exact HC routes the
  emulator implements. Statements execute on the agent via the native path.
- **SQL agent.** dbt submits Spark **SQL** (`kind: sql`) and parses the Livy SQL
  result envelope. The general-purpose Python REPL agent (`e2e/livy/agent.py`)
  execs Python and returns `text/plain` — incompatible — so this harness ships
  `sql_agent.py`, which runs `spark.sql(code)` and returns
  `data["application/json"] = {schema, data}`. Delta extensions are on so dbt's
  default `USING delta` tables build.
- **TLS.** dbt requires an HTTPS endpoint, so the emulator serves TLS on `:9443`
  here; its self-signed cert (SAN `api.fabric.microsoft.com`) is shared to the
  dbt container via the `emu-data` volume for `REQUESTS_CA_BUNDLE`.

## Milestones

- **A (this harness).** Protocol conformance: auth + Fabric REST + Livy HC +
  real Spark SQL execution. dbt's Delta tables land in the agent's *local* Spark
  warehouse; because one SparkSession serves every statement, `run`/`test`
  round-trip correctly.
- **B (follow-up).** Storage fidelity: bind the session to the lakehouse so the
  Delta lands in **OneLake** over ABFS (the agent already has the ABFS jars +
  entra token provider), matching real Fabric byte-for-byte.
