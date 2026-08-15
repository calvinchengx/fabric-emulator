"""JVM session configs are applied without starting Spark.

agent.py calls getOrCreate() at import, so the wiring lives in jvmconf.py —
the same split as catalog.py. A classic JVM session that lacks the Delta
extension fails contract-4 saveAsTable with
DELTA_CONFIGURE_SPARK_SESSION_WITH_EXTENSION_AND_CATALOG, which measures
the harness, not the engine.
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "spark_agent"))

import jvmconf  # noqa: E402


def test_delta_extension_and_catalog_are_always_set():
    pairs = dict(jvmconf.configs({}))
    assert pairs["spark.sql.extensions"] == "io.delta.sql.DeltaSparkSessionExtension"
    assert pairs["spark.sql.catalog.spark_catalog"] == (
        "org.apache.spark.sql.delta.catalog.DeltaCatalog")


def test_abfs_entra_provider_is_wired_when_token_env_is_present():
    pairs = dict(jvmconf.configs({
        "ENTRA_TOKEN_URL": "http://entra-emulator:8443/t/oauth2/v2.0/token",
        "ENTRA_CLIENT_ID": "client",
        "ENTRA_CLIENT_SECRET": "secret",
        "AZURE_ALLOW_HTTP": "true",
    }))
    account = jvmconf.DFS_ACCOUNT
    assert pairs["spark.hadoop.fs.azure.always.use.https"] == "false"
    assert pairs["spark.hadoop.fs.azure.account.auth.type." + account] == "Custom"
    assert pairs["spark.hadoop.fs.azure.account.oauth.provider.type." + account] == (
        "com.calvinchengx.fabricemu.EntraTokenProvider")
    assert pairs["spark.hadoop.fs.azure.emu.token.endpoint"].endswith("/oauth2/v2.0/token")
    assert pairs["spark.hadoop.fs.azure.emu.client.id"] == "client"
    assert pairs["spark.hadoop.fs.azure.emu.client.secret"] == "secret"
    assert pairs["spark.hadoop.fs.azure.emu.scope"] == "https://storage.azure.com/.default"


def test_abfs_is_omitted_without_entra_credentials():
    keys = [k for k, _ in jvmconf.configs({"AZURE_ALLOW_HTTP": "true"})]
    assert all(not k.startswith("spark.hadoop.fs.azure.") for k in keys)


def test_configure_applies_each_pair_to_the_builder():
    applied = []

    class Builder:
        def config(self, key, value):
            applied.append((key, value))
            return self

    out = jvmconf.configure(Builder(), {})
    assert out is not None
    assert applied == list(jvmconf.DELTA)
