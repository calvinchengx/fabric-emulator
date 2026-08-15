"""Classic JVM SparkSession configs: Delta extension/catalog and OneLake ABFS.

Split out of agent.py so it is importable without starting Spark (agent.py
calls getOrCreate() at import). The JVM overlay's spark-submit used to
create a session with neither, and contract-4 `saveAsTable` died on
DELTA_CONFIGURE_SPARK_SESSION_WITH_EXTENSION_AND_CATALOG — measuring the
harness, not the engine. Same wiring as e2e/spark-jvm/job.py and
e2e/engine-matrix/probes.py.

bindDefaultLakehouse writes `abfs://…@onelake.dfs.fabric.microsoft.com/…`,
so the Hadoop account name is that host, not AZURE_STORAGE_ACCOUNT_NAME
(Sail's object_store name, `onelake`).
"""

DFS_ACCOUNT = "onelake.dfs.fabric.microsoft.com"

DELTA = (
    ("spark.sql.extensions", "io.delta.sql.DeltaSparkSessionExtension"),
    ("spark.sql.catalog.spark_catalog", "org.apache.spark.sql.delta.catalog.DeltaCatalog"),
)


def configs(env):
    """Return (key, value) pairs for a classic JVM SparkSession.builder."""
    out = list(DELTA)
    token_url = (env.get("ENTRA_TOKEN_URL") or "").strip()
    client_id = (env.get("ENTRA_CLIENT_ID") or "").strip()
    client_secret = (env.get("ENTRA_CLIENT_SECRET") or "").strip()
    if not (token_url and client_id and client_secret):
        return out
    if (env.get("AZURE_ALLOW_HTTP") or "").lower() in ("1", "true", "yes"):
        out.append(("spark.hadoop.fs.azure.always.use.https", "false"))
    scope = (env.get("ENTRA_STORAGE_SCOPE") or "https://storage.azure.com/.default").strip()
    out.extend((
        ("spark.hadoop.fs.azure.account.auth.type." + DFS_ACCOUNT, "Custom"),
        ("spark.hadoop.fs.azure.account.oauth.provider.type." + DFS_ACCOUNT,
         "com.calvinchengx.fabricemu.EntraTokenProvider"),
        ("spark.hadoop.fs.azure.emu.token.endpoint", token_url),
        ("spark.hadoop.fs.azure.emu.client.id", client_id),
        ("spark.hadoop.fs.azure.emu.client.secret", client_secret),
        ("spark.hadoop.fs.azure.emu.scope", scope),
    ))
    return out


def configure(builder, env):
    """Apply configs() to a SparkSession.builder and return it."""
    for key, value in configs(env):
        builder = builder.config(key, value)
    return builder
