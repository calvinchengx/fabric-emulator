"""Endpoints, tokens, and shared state for the medallion examples.

**The target is resolved here and nowhere else.** `FABRIC_TARGET=emulator|real`
(default `emulator`) picks the whole coherent set — API root, token authority,
credential, OneLake, vault, TLS — through the published `fabric-target`
contract. No step branches on which target is active; they hold display NAMES
and receive endpoints, because ids can never match across targets
(docs/21-real-fabric-toggle.md).

This file is the CONSUMER half. It does not restate the contract: contoso-data-platform
did that while `fabric-target` was unpublished and the restatement drifted into
requiring a client secret, which broke `az login`, managed identity, and running
inside a Fabric notebook. A contract you copy is a contract you get wrong.

What stays local policy, because it is genuinely this example's choice rather
than the toggle's: who plays the Spark pool (`SPARK_REMOTE`), the TDS endpoint,
and which address the *emulator* must use to reach the vault it resolves
server-side (`KV_INTERNAL`).

Artifacts are persisted the way real Fabric persists them — definition parts
with Fabric's own paths (`notebook-content.py`, `pipeline-content.json`). See
docs/46-artifact-persistence.md.
"""
import collections
import json
import os
import pathlib

import requests
import urllib3
from fabric_target import apply_notebook_env, target

urllib3.disable_warnings()  # the family serves self-signed TLS

_T = target()

# The notebook runtime context, from the SAME resolved target. Steps that use
# `notebookutils` (the brokered path a Fabric notebook actually takes) would
# otherwise read the shim's own defaults — a different entra port, TLS
# verification on against the family's self-signed certs — and disagree with the
# control-plane calls in the same process. Anything already exported wins.
apply_notebook_env(_T)

# Resolved per target, never hardcoded. In real mode `fabric-target` refuses the
# emulator's seeded principal BY VALUE, so a shell left over from a local run
# cannot quietly authenticate as a dev daemon against production.
ENTRA = _T.entra_url
FABRIC = _T.api_root[: -len("/v1")] if _T.api_root.endswith("/v1") else _T.api_root
# NO FALLBACK. Under `emulator` fabric-target already defaults this to the local
# vault; under `real` it is None unless FABRIC_VAULT_URL/AZURE_KEY_VAULT_URL is
# set, and `or "https://localhost:8444"` would have pointed a PRODUCTION run at
# the emulator's vault — silently, since the resolver is exempt from the gate's
# localhost rule. So it is None, and require_vault() refuses AT THE POINT OF USE.
KV = _T.vault_url
TENANT = _T.tenant


def require_vault():
    """The vault URI, refused under `real` when nothing configured one.

    WHY NOT AT IMPORT. It was, and that was wrong: this module is imported by
    every step, so a vault nobody in that step touches blocked steps that only
    need the control plane. `provision.py` — the one step the real-Fabric CI leg
    runs, and the first thing anyone points at a fresh tenant — died on a missing
    Key Vault before making a single call. A prerequisite belongs where it is
    needed, or the toggle is narrower than it claims.
    """
    if not KV:
        raise SystemExit(
            "FABRIC_TARGET=real needs a vault for this step: set FABRIC_VAULT_URL "
            "(or AZURE_KEY_VAULT_URL) to your Key Vault URI. The step stores the "
            "source API key there and reads it back the way a notebook does. "
            "Steps that only touch the control plane (provision.py) need none.")
    return KV

# Local policy, not target policy (see the module docstring). UNSET means
# "ask the API", which is the portable path — see sql_endpoint(). It stays
# overridable because the emulator advertises the port it LISTENS on, not the
# port Docker published it to, so an isolated stack with a remapped port
# (`11533:1433`) is the one case discovery gets wrong. Fabric has no such
# distinction, which is why this is an override and not a parameter.
TDS_SERVER = os.environ.get("TDS_SERVER")
KV_INTERNAL = os.environ.get("KV_INTERNAL_URL", "https://keyvault-emulator:8444")
SPARK_REMOTE = os.environ.get("SPARK_REMOTE", "sc://localhost:50051")
# OpenMetadata, the governance sidecar the advanced examples catalog into. Local
# policy for the same reason: it is a companion service, not a Fabric surface, so
# the toggle has nothing to say about it. Real Fabric's counterpart is Purview,
# which is a different integration rather than a different endpoint.
OM_URL = os.environ.get("OM_URL", "http://localhost:8585")

FABRIC_AUD = "https://api.fabric.microsoft.com"
STORAGE_AUD = "https://storage.azure.com"
SQL_AUD = "https://database.windows.net"
VAULT_AUD = "https://vault.azure.net"
PBI_AUD = "https://analysis.windows.net/powerbi/api"

S = _T.session()

# entra-emulator's own endpoints (the admin API, the token endpoint) must NOT
# carry a Fabric bearer: `S` injects one on every request, which is correct for
# the control plane and meaningless to the authority issuing the token. So the
# few calls that talk to the issuer use a plain session with the target's TLS
# mode, not the authenticated one.
_RAW = requests.Session()
_RAW.verify = _T.tls_verify

# Anchored on the CALLING example's directory, not on this file's. This module
# is shared by both medallion examples, so `HERE` would resolve to the fixture
# package — and state.json would be written inside site-packages, with every
# example silently sharing one. Each example is run from its own directory, so
# the working directory is the right anchor.
HERE = pathlib.Path.cwd()
STATE = pathlib.Path(os.environ.get("PIPELINE_STATE", HERE / "state.json"))
GOLD_PROJECT = os.environ.get("GOLD_PROJECT", str(HERE / "gold"))
# Display names are unique per emulator, so two examples provisioning against
# one stack must not both ask for "contoso-analytics". examples/medallion-dbt-fabricspark
# overrides this; nothing else needs to.
WORKSPACE_NAME = os.environ.get("WORKSPACE_NAME", "contoso-analytics")


def log(msg):
    print(f"==> {msg}", flush=True)


def ensure_app(app_id_uri, name):
    """Register a non-default audience in entra (409 = already there).

    SKIPPED under `real`, not refused — and the distinction matters.

    This is emulator-only SETUP, not an emulator-only FEATURE. It POSTs
    entra-emulator's admin API to make an audience issuable; in a real tenant the
    audiences these examples ask for (storage, vault, SQL, Power BI) are Microsoft
    first-party resources that already exist, so the POSTCONDITION is already
    true and there is nothing to do. Refusing here would turn "no setup needed"
    into "this step cannot run", which is what it did at first — and it blocked
    four of the eleven steps from ever being portable.

    Contrast `_emulator/clock`: advancing time is a CAPABILITY with no real
    counterpart, so a caller that needs it is asking for something production
    cannot give. That one must refuse (see contoso-data-platform's
    platform/schedule.py, which asserts clock_is_controllable).

    The rule: if the target already satisfies the postcondition, skip; if the
    target cannot satisfy it at all, refuse.
    """
    if _T.is_real:
        log(f"skipping ensure_app({app_id_uri}) — a real tenant already issues it")
        return
    r = _RAW.post(f"{ENTRA}/admin/api/apps",
               json={"displayName": name, "appIdUri": app_id_uri, "isConfidential": False})
    assert r.status_code in (200, 201, 409), f"seed {app_id_uri}: {r.status_code} {r.text}"


def skip_on_real(step, because):
    """Exit 0 under `real`: there is nothing for this step to do there.

    The step-level form of ensure_app's skip, and it covers two cases that share
    one answer. Either real Fabric reaches the postcondition itself (it runs the
    notebook on its own pool, so the local engine has no job), or the step drives
    a companion service that Fabric simply has no endpoint for (OpenMetadata;
    Purview is a different integration, not a different URL). Both times the work
    the pipeline needs is not missing, so failing would be a lie — it exits
    successfully and says why.

    Contrast only_the_emulator_can, for a step whose OUTPUT the pipeline does
    need and cannot get.
    """
    if _T.is_real:
        log(f"skipping {step}: {because}")
        raise SystemExit(0)


def only_the_emulator_can(step, because, instead):
    """Refuse under `real`, naming the mechanism real Fabric uses instead.

    For a step that depends on a CAPABILITY the emulator adds for local
    development and real Fabric does not expose. The refusal is the honest
    answer, and `instead` is what makes it actionable rather than a dead end:
    the portable form exists, it is just a different shape.
    """
    if _T.is_real:
        raise SystemExit(
            f"{step} cannot run against real Fabric.\n"
            f"  why:     {because}\n"
            f"  instead: {instead}")


def token(audience):
    """A bearer for `audience`, from whichever credential the target resolved:
    the seeded daemon locally, DefaultAzureCredential (env SP, managed identity,
    or a developer's `az login`) under real."""
    return _T.credential.get_token(audience + "/.default").token


def fabric_headers():
    return {"Authorization": "Bearer " + token(FABRIC_AUD)}


def storage_options():
    """delta-rs options pointing at OneLake's account-prefixed Blob path."""
    opts = {
        "azure_storage_account_name": "onelake",
        "azure_storage_token": token(STORAGE_AUD),
        "azure_endpoint": f"{_T.onelake_url}/onelake",
    }
    if _T.onelake_url.startswith("http://"):
        opts["azure_allow_http"] = "true"
    else:
        opts["azure_allow_invalid_certificates"] = "true"
    return opts


def tables_uri():
    st = load()
    return f"az://{st['workspace']}/{st['lakehouse']}/Tables"


# An item's SQL endpoint, as the API reports it. The collection segment is the
# typed route Fabric documents the property on — the generic /items/{id} answers
# the generic record there, so reading the address off it can work against a
# development stack and return nothing on real Fabric
# (docs/46-artifact-persistence.md).
_SQL_COLLECTION = {"Warehouse": "warehouses", "SQLDatabase": "sqlDatabases",
                   "Lakehouse": "lakehouses"}
SqlTarget = collections.namedtuple("SqlTarget", "server database encrypt endpoint_id")
_SQL_CACHE = {}


def _odbc_server(connection_string):
    """`host:port` (what the emulator advertises) -> `host,port` (what the SQL
    Server ODBC driver wants). A real Fabric connection string is a bare FQDN on
    the default port and passes through untouched; an IPv6 literal keeps its
    brackets, so only a trailing numeric port is rewritten."""
    host, sep, port = connection_string.rpartition(":")
    if sep and port.isdigit():
        return f"{host},{port}"
    return connection_string


def sql_endpoint(item_id):
    """Where to dial for `item_id`'s SQL endpoint, and as what database.

    THIS IS THE PORTABLE FORM, and the reason is not convenience. On real Fabric
    the SQL address is per-workspace and assigned by the service: nothing outside
    it can know the host, so any example that hardcodes one is emulator-only by
    construction, however carefully everything else is resolved. Both targets
    answer the same documented question instead:

      Warehouse / SQL database -> properties.connectionString
      Lakehouse                -> properties.sqlEndpointProperties.connectionString
                                  (its read-only SQL analytics endpoint)

    The DATABASE NAME differs by target and that is real, not a local shortcut.
    Fabric addresses a database by DISPLAY NAME and encodes the workspace in the
    server name; a development stack serves one host for every workspace, so
    there it is addressed by item id — both spellings are accepted locally.
    Resolving it here is what keeps the branch out of the steps.

    Cached per item: the callers are retry loops waiting for a database to come
    online, and an endpoint does not move between attempts.
    """
    if item_id in _SQL_CACHE:
        return _SQL_CACHE[item_id]
    st = load()
    H = fabric_headers()
    base = f"{FABRIC}/v1/workspaces/{st['workspace']}"
    r = S.get(f"{base}/items/{item_id}", headers=H)
    r.raise_for_status()
    item = r.json()
    collection = _SQL_COLLECTION.get(item["type"])
    assert collection, f"item {item['displayName']!r} is a {item['type']}, which has no SQL endpoint"

    # The typed route is asked even when TDS_SERVER overrides the address: the
    # endpoint id is a different fact from the host, and sync_sql_endpoint needs it.
    server, endpoint_id = TDS_SERVER, None
    r = S.get(f"{base}/{collection}/{item_id}", headers=H)
    r.raise_for_status()
    props = r.json().get("properties") or {}
    if item["type"] == "Lakehouse":
        ep = props.get("sqlEndpointProperties") or {}
        # Real Fabric's analytics endpoint is a SQLEndpoint item with its own
        # id, which is what refreshMetadata is addressed by. The emulator has
        # no such item and reflects on connect, so it reports no id and
        # sync_sql_endpoint takes the other branch.
        endpoint_id = ep.get("id")
        # Real Fabric provisions the analytics endpoint asynchronously, so a
        # status that is not Success is a wait-or-fail, not a missing feature.
        # Saying which beats a connection timeout that names nothing. Only
        # when the property is THERE, though: absent means this build serves
        # no SQL at all, which the no-endpoint branch below explains better.
        if ep:
            assert ep.get("provisioningStatus") == "Success", (
                f"the SQL analytics endpoint of {item['displayName']!r} is "
                f"{ep['provisioningStatus']}; it cannot be queried yet")
        server = server or ep.get("connectionString")
    else:
        server = server or props.get("connectionString")
    assert server, (
        f"{item['type']} {item['displayName']!r} advertises no SQL endpoint. "
        f"Locally that means the stack runs without its SQL sidecar; set "
        f"TDS_SERVER to override.")
    server = _odbc_server(server)

    _SQL_CACHE[item_id] = SqlTarget(
        server=server,
        endpoint_id=endpoint_id,
        database=item["displayName"] if _T.is_real else item_id,
        # Real Fabric's endpoint requires TLS. The emulator's TDS front
        # terminates FedAuth without it (it advertises ENCRYPT_NOT_SUP).
        encrypt=_T.is_real)
    return _SQL_CACHE[item_id]


def sync_sql_endpoint(item_id):
    """Make a lakehouse's SQL analytics endpoint see the CURRENT Delta state.

    THE MECHANISM DIFFERS BY TARGET, AND THAT IS THE WHOLE REASON THIS EXISTS.

    The emulator reflects on connect: opening a session makes it read each
    `Tables/<t>` Delta and rebuild the SQL view, so "connect" and "sync" are the
    same act. Steps relied on that — `dq_gate.py` opened a throwaway connection
    with the comment "re-reflect whatever silver now holds" — and on real Fabric
    that is a NO-OP with a plausible shape: the analytics endpoint syncs on its
    own schedule, normally under a minute, so a table Spark just wrote can be
    briefly absent and the step reads stale data instead of failing. A silent
    wrong answer, which is the worst kind.

    Real Fabric's explicit lever is
    `POST /v1/workspaces/{ws}/sqlEndpoints/{sqlEndpointId}/refreshMetadata`, and
    it is addressed by the ENDPOINT's id — a separate SQLEndpoint item, which the
    emulator does not have and does not report. So the absence of that id is
    exactly the signal that the connect-to-reflect path is the right one here.
    """
    t = sql_endpoint(item_id)
    if not t.endpoint_id:
        with tds_connect(item_id):  # connecting IS the sync locally
            pass
        return
    st = load()
    r = S.post(f"{FABRIC}/v1/workspaces/{st['workspace']}/sqlEndpoints/"
               f"{t.endpoint_id}/refreshMetadata", headers=fabric_headers(), json={})
    # 200 with a per-table report, or a 202 whose operation must be followed.
    assert r.status_code in (200, 202), f"refreshMetadata: {r.status_code} {r.text}"
    _T.poll_lro(r)
    log(f"SQL analytics endpoint {t.endpoint_id} metadata refreshed")


def tds_connect(item_id, token_value=None, timeout=60):
    """FedAuth over TDS to an item's SQL endpoint: the pre-minted Azure-SQL token
    rides in SQL_COPT_SS_ACCESS_TOKEN (1256) — the exact injection dbt-fabric
    performs, so the ODBC driver never runs MSAL."""
    import struct

    import pyodbc

    t = sql_endpoint(item_id)
    enc = (token_value or token(SQL_AUD)).encode("utf-16-le")
    return pyodbc.connect(
        "DRIVER={ODBC Driver 18 for SQL Server};"
        f"SERVER={t.server};Database={t.database};"
        f"Encrypt={'yes' if t.encrypt else 'no'};TrustServerCertificate=yes",
        attrs_before={1256: struct.pack("<i", len(enc)) + enc}, timeout=timeout)


def save(**kv):
    state = load()
    state.update(kv)
    STATE.write_text(json.dumps(state, indent=2))


def load():
    return json.loads(STATE.read_text()) if STATE.exists() else {}


# --- pipeline orchestration --------------------------------------------------
# The emulator executes DataPipeline activities for real: a Copy moves the data
# itself. A Notebook activity is different — the emulator parses the notebook
# into cells and records a Pending run, then an ENGINE executes those cells and
# reports back. That split is why these helpers exist.

DEFINITIONS = pathlib.Path(os.environ.get("DEFINITIONS", HERE / "definitions"))


def create_item_from_definition(folder, display_name=None, **substitutions):
    """Create an item from an on-disk definition folder in Fabric's SOURCE FORMAT.

    `definitions/<display name>.<Type>/` holding the item's definition files plus
    `.platform` is exactly what Fabric's Git integration writes and what
    `fabric-cicd` deploys, so what is committed here is what a real repository
    committed (docs/46-artifact-persistence.md). The alternative — building the
    body in a Python string — models the API and not the artefact, and teaches a
    layout no CI/CD tool would recognise.

    WHY THERE IS A SUBSTITUTION STEP. A definition names the workspace, lakehouse
    and upstream items it reads by GUID, and those do not exist until provisioning
    has run. So the committed files carry `{{TOKENS}}` and the deploy substitutes
    them. That is not a local workaround: Microsoft's own fabric-cicd ships a
    `find_replace` parameter file for the same reason, because one definition has
    to land in dev, test and prod with different ids.

    The item TYPE comes from the folder name, as it does in Git, so a definition
    cannot be deployed as the wrong type by a typo at the call site.
    """
    folder_path = DEFINITIONS / folder
    assert folder_path.is_dir(), f"no definition folder {folder_path}"
    name, _, item_type = folder.rpartition(".")
    assert name and item_type, (
        f"definition folder {folder!r} must be named '<display name>.<Type>', "
        f"the layout Fabric's Git integration uses")

    parts = {}
    for path in sorted(folder_path.rglob("*")):
        if not path.is_file():
            continue
        text = path.read_text()
        for token, value in substitutions.items():
            text = text.replace("{{" + token + "}}", str(value))
        # A surviving placeholder would deploy cleanly and fail much later as a
        # Spark error about a workspace literally named {{WORKSPACE_ID}}.
        assert "{{" not in text, (
            f"unsubstituted placeholder in {path.relative_to(folder_path)}: "
            f"{text[text.index('{{'):][:40]!r}")
        parts[str(path.relative_to(folder_path)).replace(os.sep, "/")] = text

    return create_item(display_name or name, item_type, parts)


def post_and_wait(url, body):
    """POST a create and return the created object, resolving a 202 if there is one.

    FOUND BY RUNNING IT ON A REAL TENANT, which is the only way this class of bug
    surfaces. Real Fabric creates a **Warehouse asynchronously**: it answers 202
    with an operation id and NO body, where the emulator answers 201 with the item.
    `provision.py` assumed the 201 and did `None["id"]` against a trial — after
    the workspace and lakehouse had already been created, so the failure was three
    calls away from the assumption that caused it.

    Every Fabric create can be async; which ones ARE is a service decision that
    can change. So this resolves the operation whenever one is offered rather than
    per item type, which is also what fabric-cicd does.
    """
    import time

    H = fabric_headers()
    r = S.post(url, headers=H, json=body)
    assert r.status_code in (200, 201, 202), f"{url} -> {r.status_code} {r.text}"
    if r.status_code != 202:
        return r.json()
    op = r.headers.get("x-ms-operation-id") \
        or r.headers["Location"].rstrip("/").rsplit("/", 1)[-1]
    for _ in range(150):
        got = S.get(f"{FABRIC}/v1/operations/{op}", headers=H)
        status = (got.json() or {}).get("status")
        if status in ("Succeeded", "Failed"):
            break
        # Honour the service's own pacing when it states one: real creates take
        # real seconds, and polling faster than asked earns a 429.
        time.sleep(min(float(got.headers.get("Retry-After", "2")), 20))
    else:
        raise SystemExit(f"{url}: operation {op} never reached a terminal state")
    assert status == "Succeeded", f"{url}: operation {op} {status}"
    return S.get(f"{FABRIC}/v1/operations/{op}/result", headers=H).json()


def create_item(display_name, item_type, parts):
    """Create an item with a definition, resolving the LRO if the create is async."""
    import base64
    import time

    H = fabric_headers()
    st = load()
    body = {"displayName": display_name, "type": item_type, "definition": {"parts": [
        {"path": p, "payloadType": "InlineBase64",
         "payload": base64.b64encode(c.encode() if isinstance(c, str) else c).decode()}
        for p, c in parts.items()]}}
    r = S.post(f"{FABRIC}/v1/workspaces/{st['workspace']}/items", headers=H, json=body)
    assert r.status_code in (201, 202), f"create {item_type}: {r.status_code} {r.text}"
    if r.status_code == 201:
        return r.json()["id"]
    op = r.headers["x-ms-operation-id"]
    for _ in range(60):
        status = S.get(f"{FABRIC}/v1/operations/{op}", headers=H).json()["status"]
        if status in ("Succeeded", "Failed"):
            break
        time.sleep(1)
    assert status == "Succeeded", f"create {item_type}: operation {status}"
    return S.get(f"{FABRIC}/v1/operations/{op}/result", headers=H).json()["id"]


def run_job(item_id, job_type, body=None):
    """Start a job and wait for it to reach a terminal state. Returns (job_id, status)."""
    import time

    H = fabric_headers()
    st = load()
    base = f"{FABRIC}/v1/workspaces/{st['workspace']}/items/{item_id}/jobs/instances"
    r = S.post(f"{base}?jobType={job_type}", headers=H, json=body)
    assert r.status_code in (200, 202), f"run {job_type}: {r.status_code} {r.text}"
    jid = r.headers["Location"].rsplit("/", 1)[-1]
    for _ in range(120):
        body = S.get(f"{base}/{jid}", headers=H).json()
        status = body.get("status")
        if status in ("Completed", "Failed", "Cancelled"):
            if status == "Failed":
                # A bare "Failed" is useless to a reader. Surface which activity
                # broke and why, which the interpreter already recorded.
                detail = body.get("failureReason", {})
                try:
                    for r in activity_runs(item_id, jid):
                        if r.get("status") != "Succeeded":
                            log(f"activity {r.get('activityName')!r} {r.get('status')}: {r.get('error')}")
                except Exception:  # noqa: BLE001 — diagnostics must not mask the failure
                    pass
                log(f"{job_type} job {jid} failed: {detail}")
            return jid, status
        time.sleep(1)
    raise SystemExit(f"{job_type} job {jid} never reached a terminal state")


def activity_runs(item_id, job_id):
    """The per-activity results the interpreter recorded for a pipeline run."""
    st = load()
    r = S.post(f"{FABRIC}/v1/workspaces/{st['workspace']}/items/{item_id}"
               f"/jobs/instances/{job_id}/queryactivityruns", headers=fabric_headers(), json={})
    r.raise_for_status()
    return r.json().get("value", r.json())


def lineage_edges():
    """Every data-movement edge the emulator recorded for this workspace."""
    st = load()
    r = S.get(f"{FABRIC}/v1/workspaces/{st['workspace']}/lineage", headers=fabric_headers())
    r.raise_for_status()
    return r.json().get("value", [])


def report_lineage(step, moves):
    """Tell the emulator what this step moved, so it appears in the flow graph.

    A queued notebook run reports its read/write set when it finishes, and the
    emulator records the OneLake bytes it sees either way. What it cannot see is
    which read CAUSED which write — an interactive Spark session or a plain
    script leaves no such trace, and the emulator will not guess. So the step
    says so itself, and the edge is marked `Reported` to distinguish a claim
    from something the emulator watched happen.

    `moves` is a list of (reads, writes) pairs, each a real derivation. It is a
    LIST rather than one reads/writes pair because pairing them as a cross
    product overstates: silver reads bronze_customers and bronze_orders and
    writes three tables, but the quarantine comes from the orders alone. Six
    edges, three of them describing movements that never happened. Report the
    groups the code actually computes.

    Each read/write is an (item_id, path) pair, e.g. (lakehouse, "Tables/x").

    SKIPPED under `real`, because there is nothing to tell. The flow graph is the
    emulator's own record of what moved through it; real Fabric publishes no
    endpoint that accepts a claim like this, and Purview — its answer to the same
    question — is a different integration with a different model, not this call to
    a different host. Skipping rather than refusing for the reason lineage.py
    gives: the step's real work is the transform, and a graph the target does not
    keep is not a result the pipeline needs.
    """
    if _T.is_real:
        log(f"skipping the {step} lineage report: the flow graph is the emulator's "
            f"own record of what moved, and Purview is Fabric's counterpart")
        return
    st = load()
    body = {"step": step, "moves": [
        {"reads": [{"itemId": i, "path": p} for i, p in reads],
         "writes": [{"itemId": i, "path": p} for i, p in writes]}
        for reads, writes in moves]}
    r = S.post(f"{FABRIC}/v1/workspaces/{st['workspace']}/lineage",
               headers=fabric_headers(), json=body)
    assert r.status_code == 200, f"report lineage: {r.status_code} {r.text}"
    log(f"lineage: {step} recorded {r.json()['edgesRecorded']} edge(s)")
