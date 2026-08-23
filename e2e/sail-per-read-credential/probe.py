#!/usr/bin/env python3
"""Does Sail honour a storage credential passed PER OPERATION, or only its own?

THE QUESTION DECIDES AN ARCHITECTURE, so it is asked before anything is built on
the answer. Closing the last OneLake security gap means taking the ambient
`AZURE_STORAGE_TOKEN` away from the engine, so a bare
`spark.read.load("abfss://...")` in a notebook has no identity to read with.
That is only worth costing out if the reads we want to KEEP can carry their own.

Two engines: `sail` has a credential and seeds the table, `sail-nocred` has none
and is what every case below reads through. Writing is the first thing that
breaks without a credential, so seeding through the same engine would measure
nothing.

Cases, in the order that makes a failure interpretable:

  1. the seed, through the engine WITH a credential -> the control. If this
     fails the stack is broken and nothing after it means anything.
  2. no options, through the engine without one     -> must fail; that is the
     property the whole idea depends on.
  3. the credential passed as a read OPTION         -> THE ANSWER.
  4. the credential set as a session CONF           -> the fallback spelling,
     in case options are not the channel Sail reads.
"""
import os
import sys
import time
import urllib.parse
import urllib.request

from pyspark.sql import SparkSession

REMOTE = os.environ["SPARK_REMOTE"]
SEED_REMOTE = os.environ["SEED_REMOTE"]
FABRIC = "http://fabric-emulator"
ENTRA = "http://entra-emulator:8443"
TENANT = "6f89cf12-978b-4d23-ac18-9ef0c127cf87"


def post(url, body, form=False):
    if form:
        data = urllib.parse.urlencode(body).encode()
        headers = {"Content-Type": "application/x-www-form-urlencoded"}
    else:
        import json as _json
        data = _json.dumps(body).encode()
        headers = {"Content-Type": "application/json"}
    req = urllib.request.Request(url, data=data, method="POST", headers=headers)
    with urllib.request.urlopen(req, timeout=60) as r:
        import json as _json
        raw = r.read()
        return _json.loads(raw) if raw else {}


def wait_health():
    for _ in range(90):
        try:
            with urllib.request.urlopen(f"{FABRIC}/health", timeout=2) as r:
                if r.status == 200:
                    return
        except OSError:
            pass
        time.sleep(1)
    raise RuntimeError("fabric-emulator never came up")


def attempt(label, fn):
    started = time.time()
    try:
        value = fn()
        print(f"  {label}\n      -> {value}   ({time.time() - started:.0f}s)", flush=True)
        return value
    except Exception as exc:  # noqa: BLE001 - the exception IS the result
        line = str(exc).strip().splitlines()[0][:120]
        print(f"  {label}\n      -> {type(exc).__name__}: {line}"
              f"   ({time.time() - started:.0f}s)", flush=True)
        return None


def main():
    wait_health()
    api = post(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials",
        "client_id": "00d88624-f0d7-46f6-a641-6232c2608928",
        "client_secret": "daemon-app-secret",
        "scope": "https://api.fabric.microsoft.com/.default"}, form=True)["access_token"]
    storage = post(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials",
        "client_id": "00d88624-f0d7-46f6-a641-6232c2608928",
        "client_secret": "daemon-app-secret",
        "scope": "https://storage.azure.com/.default"}, form=True)["access_token"]

    req = urllib.request.Request(
        f"{FABRIC}/v1/workspaces", method="POST",
        data=b'{"displayName":"percred-ws"}',
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + api})
    import json as _json
    with urllib.request.urlopen(req, timeout=60) as r:
        ws = _json.loads(r.read())
    req = urllib.request.Request(
        f"{FABRIC}/v1/workspaces/{ws['id']}/lakehouses", method="POST",
        data=b'{"displayName":"lake"}',
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + api})
    with urllib.request.urlopen(req, timeout=60) as r:
        lake = _json.loads(r.read())

    path = (f"abfss://{ws['id']}@onelake.dfs.fabric.microsoft.com"
            f"/{lake['id']}/Tables/sales")
    opts = {
        "azure_storage_account_name": "onelake",
        "azure_endpoint": os.environ["AZURE_STORAGE_ENDPOINT"],
        "azure_allow_http": "true",
        "azure_storage_token": storage,
    }

    print("1. seed, through the engine WITH a credential (the control)", flush=True)
    seeder = SparkSession.builder.remote(SEED_REMOTE).create()
    wrote = attempt("write 3 rows",
                    lambda: (seeder.createDataFrame([(1, 10), (1, 20), (2, 30)],
                                                    ["region_id", "amount"])
                             .write.format("delta").mode("overwrite").save(path), "ok")[1])
    if wrote is None:
        print("\nthe control failed: nothing below is interpretable", flush=True)
        return 0
    print(f"   seeded, and readable through it: "
          f"{seeder.read.format('delta').load(path).count()} rows", flush=True)

    spark = SparkSession.builder.remote(REMOTE).create()
    print("\n2. through the engine with NO credential", flush=True)
    attempt("no options at all", lambda: spark.read.format("delta").load(path).count())

    print("\n3. THE ANSWER: the credential passed as a read option", flush=True)
    attempt("read.options(azure_storage_token=...)",
            lambda: spark.read.format("delta").options(**opts).load(path).count())

    print("\n4. the fallback spelling: a session conf", flush=True)

    def via_conf():
        for k, v in opts.items():
            spark.conf.set(f"spark.sql.catalog.{k}", v)
            spark.conf.set(k, v)
        return spark.read.format("delta").load(path).count()

    attempt("conf then read", via_conf)
    return 0


if __name__ == "__main__":
    sys.exit(main())
