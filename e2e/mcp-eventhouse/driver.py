#!/usr/bin/env python3
"""Fabric's remote Eventhouse MCP server, driven by the unmodified Python MCP
SDK, as users, against kustainer.

The server's contract is the one two third parties captured from Fabric's live
tools/list (cited in internal/api/ehmcp.go): serverInfo KustoMCP 1.0.0 and four
tools, executeQuery(kqlQuery, maxRecords, …) answering a Kusto document whose
rows are in PrimaryResult, and three grounding tools taking referenceText that
refuse an empty database with "Database is empty". This driver:

  1. signs in entra-emulator's seeded users with the password grant;
  2. as Alice (workspace Admin), creates an eventhouse, a table in its default
     KQL database through the emulator's Kusto endpoint, and a second, empty
     KQL database, and makes Bob a workspace Viewer;
  3. as Alice on the endpoint scoped to the default database, drives all four
     tools against the real engine, including a failing query;
  4. as Bob on the global endpoint, queries the same table by workspaceId and
     itemId, and is redirected to the empty database with clusterUrl and
     databaseName;
  5. shows a caller who cannot read the database finds nothing.
"""
import asyncio
import base64
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

import httpx2
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client

FABRIC = os.environ["FABRIC_BASE"]
ENTRA = os.environ["ENTRA_BASE"]
TENANT = os.environ.get("TENANT", "6f89cf12-978b-4d23-ac18-9ef0c127cf87")
CLIENT_ID = "00d88624-f0d7-46f6-a641-6232c2608928"
CLIENT_SECRET = "daemon-app-secret"
FABRIC_SCOPE = "https://api.fabric.microsoft.com/.default"
KUSTO_AUDIENCE = "https://kusto.fabric.microsoft.com"

# entra-emulator's seeded users (its docs/06-data-model-and-seed.md).
ALICE = "alice@entraemulator.dev"
BOB = "bob@entraemulator.dev"
BOB_OID = "0d4ba1f9-cab1-4200-b516-d4cb8b340930"
SEED_PASSWORD = "Password1!"

_CTX = ssl.create_default_context()
_CTX.check_hostname = False
_CTX.verify_mode = ssl.CERT_NONE

failures = []


def check(ok, what, detail=""):
    print(f"  {'PASS' if ok else 'FAIL'}  {what}{(' — ' + detail) if detail else ''}", flush=True)
    if not ok:
        failures.append(what)


def http(method, url, body=None, token=None, form=False):
    data, hdrs = None, {"Accept": "application/json"}
    if body is not None:
        if form:
            data = urllib.parse.urlencode(body).encode()
        else:
            data = json.dumps(body).encode()
            hdrs["Content-Type"] = "application/json"
    if token:
        hdrs["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
    try:
        with urllib.request.urlopen(req, context=_CTX, timeout=300) as r:
            raw = r.read()
            return r.status, {k.lower(): v for k, v in r.headers.items()}, json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, {k.lower(): v for k, v in e.headers.items()}, json.loads(raw) if raw else None
        except ValueError:
            return e.code, {}, raw.decode(errors="replace")


def user_token(upn):
    status, _, body = http("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
        "grant_type": "password", "client_id": CLIENT_ID, "client_secret": CLIENT_SECRET,
        "username": upn, "password": SEED_PASSWORD, "scope": FABRIC_SCOPE}, form=True)
    if status != 200:
        raise SystemExit(f"token for {upn}: {status} {body}")
    return body["access_token"]


def oid_of(jwt):
    payload = jwt.split(".")[1]
    return json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))["oid"]


def kusto_token_as(oid):
    """entra-emulator's admin mint, on the Kusto audience, for this user's oid:
    the seeded directory carries no Kusto resource principal to sign in to."""
    status, _, body = http("POST", f"{ENTRA}/admin/api/tokens", {
        "clientId": CLIENT_ID, "audience": KUSTO_AUDIENCE, "extraClaims": {"oid": oid, "sub": oid}})
    if status != 200:
        raise SystemExit(f"kusto token: {status} {body}")
    return body.get("access_token") or body["token"]


def create(tok, path, body):
    status, hdrs, out = http("POST", f"{FABRIC}{path}", body, token=tok)
    if status in (200, 201):
        return out
    if status != 202:
        raise SystemExit(f"POST {path}: {status} {out}")
    opid = hdrs.get("x-ms-operation-id")
    for _ in range(300):
        state = http("GET", f"{FABRIC}/v1/operations/{opid}", token=tok)[2]
        if state.get("status") == "Succeeded":
            return http("GET", f"{FABRIC}/v1/operations/{opid}/result", token=tok)[2]
        time.sleep(0.2)
    raise SystemExit(f"POST {path} did not complete")


def kusto(query_uri, tok, kind, db, csl):
    status, _, out = http("POST", f"{query_uri}/v1/rest/{kind}", {"db": db, "csl": csl}, token=tok)
    if status != 200:
        raise SystemExit(f"{kind} {csl[:60]!r}: {status} {out}")
    return out


def text_of(result):
    return result.content[0].text


def primary(result):
    """Rows of PrimaryResult, as the captured client reads them."""
    doc = json.loads(text_of(result))
    table = next(t for t in doc["Tables"] if t["TableName"] == "PrimaryResult")
    return [c["ColumnName"] for c in table["Columns"]], table["Rows"]


async def session_for(tok, path, fn):
    async with (
        httpx2.AsyncClient(headers={"Authorization": "Bearer " + tok}, timeout=300) as http_client,
        streamable_http_client(f"{FABRIC}{path}", http_client=http_client) as streams,
        ClientSession(streams[0], streams[1]) as session,
    ):
        init = await session.initialize()
        check(init.server_info.name == "KustoMCP" and init.server_info.version == "1.0.0",
              "initialize names the KustoMCP server", f"{init.server_info.name} {init.server_info.version}")
        return await fn(session)


async def as_alice(session, query_uri, db_name):
    tools = (await session.list_tools()).tools
    names = [t.name for t in tools]
    check(names == ["executeQuery", "getSchema", "getGeneralKQLExamples", "getSpecificKQLExamples"],
          "tools/list is the four captured tools", str(names))
    required = set(tools[0].input_schema.get("required", []))
    check(required == {"kqlQuery", "maxRecords"}, "executeQuery's captured required arguments", str(required))

    r = await session.call_tool("executeQuery", {
        "kqlQuery": "StormEvents | summarize Total = sum(Damage) by State | order by Total desc",
        "maxRecords": 1000, "activityTitle": "damage by state"})
    cols, rows = primary(r) if not r.is_error else ([], text_of(r))
    check(not r.is_error and cols == ["State", "Total"] and rows == [["TEXAS", 700], ["KANSAS", 50]],
          "executeQuery runs real KQL and answers PrimaryResult", f"{cols} {rows}")
    kinds = [t["TableName"] for t in json.loads(text_of(r))["Tables"]] if not r.is_error else []
    check("PrimaryResult" in kinds, "result tables are named by kind", str(kinds))

    capped = await session.call_tool("executeQuery", {"kqlQuery": "range i from 1 to 1500 step 1", "maxRecords": 5000})
    check(not capped.is_error and len(primary(capped)[1]) == 1000, "maxRecords is capped at 1,000, silently",
          str(len(primary(capped)[1])) if not capped.is_error else text_of(capped))

    bad = await session.call_tool("executeQuery", {"kqlQuery": "NoSuchTable | take 1", "maxRecords": 1})
    prefix = f"Error in executing KQL query. cluster='{query_uri}', database='{db_name}', Exception='"
    check(bad.is_error and text_of(bad).startswith(prefix) and "NoSuchTable" in text_of(bad),
          "a failed query names the cluster, the database and the engine's error", text_of(bad)[:220])

    schema = await session.call_tool("getSchema", {"referenceText": "storm damage by state"})
    doc = json.loads(text_of(schema)) if not schema.is_error else {}
    storms = next((t for t in doc.get("Tables", []) if t["Name"] == "StormEvents"), {})
    check(not schema.is_error and storms.get("RowCount") == 3 and len(storms.get("SampleRows", [])) == 3
          and [c["Name"] for c in storms.get("Columns", [])] == ["State", "EventType", "Damage"],
          "getSchema reads the real table: columns, row count and samples", json.dumps(storms)[:220])

    general = await session.call_tool("getGeneralKQLExamples", {"referenceText": "count events per hour"})
    check(not general.is_error and "```kql" in text_of(general), "getGeneralKQLExamples answers KQL examples")
    specific = await session.call_tool("getSpecificKQLExamples", {"referenceText": "anything"})
    check(not specific.is_error and "No examples" in text_of(specific),
          "getSpecificKQLExamples has none for a database it has not learned", text_of(specific))


async def as_bob(session, ws, db_id, query_uri):
    tools = (await session.list_tools()).tools
    check({"workspaceId", "itemId"} <= set(tools[0].input_schema.get("required", [])),
          "the global endpoint asks which database")
    r = await session.call_tool("executeQuery", {"workspaceId": ws, "itemId": db_id,
                                                 "kqlQuery": "StormEvents | count", "maxRecords": 10})
    check(not r.is_error and primary(r)[1] == [[3]], "Bob, a Viewer, reads the table through the global endpoint",
          text_of(r)[:200])
    empty = await session.call_tool("getSchema", {"workspaceId": ws, "itemId": db_id, "referenceText": "x",
                                                  "clusterUrl": query_uri, "databaseName": "Archive"})
    check(empty.is_error and text_of(empty) == "Database is empty",
          "clusterUrl and databaseName redirect to the empty database, which grounds nothing", text_of(empty))


async def as_stranger(session, ws, db_id):
    r = await session.call_tool("executeQuery", {"workspaceId": ws, "itemId": db_id, "kqlQuery": "print 1",
                                                 "maxRecords": 1})
    check(r.is_error and "or you cannot read it" in text_of(r), "a caller who cannot read the database finds nothing",
          text_of(r)[:160])


def main():
    alice, bob = user_token(ALICE), user_token(BOB)
    ws = create(alice, "/v1/workspaces", {"displayName": "eh-mcp-e2e"})["id"]
    create(alice, f"/v1/workspaces/{ws}/roleAssignments", {"principal": {"id": BOB_OID, "type": "User"}, "role": "Viewer"})
    eh = create(alice, f"/v1/workspaces/{ws}/eventhouses", {"displayName": "Telemetry"})["id"]
    props = http("GET", f"{FABRIC}/v1/workspaces/{ws}/eventhouses/{eh}", token=alice)[2]["properties"]
    query_uri, db_id = props["queryServiceUri"], props["databasesItemIds"][0]
    db_name = http("GET", f"{FABRIC}/v1/workspaces/{ws}/kqlDatabases/{db_id}", token=alice)[2]["displayName"]
    create(alice, f"/v1/workspaces/{ws}/kqlDatabases", {"displayName": "Archive", "creationPayload": {
        "databaseType": "ReadWrite", "parentEventhouseItemId": eh}})
    print(f"==> workspace={ws} eventhouse={eh} database={db_id} ({db_name}) queryServiceUri={query_uri}", flush=True)

    ktok = kusto_token_as(oid_of(alice))
    kusto(query_uri, ktok, "mgmt", db_name, ".create-merge table StormEvents (State:string, EventType:string, Damage:long)")
    kusto(query_uri, ktok, "mgmt", db_name,
          ".ingest inline into table StormEvents <|\nTEXAS,Hail,400\nKANSAS,Tornado,50\nTEXAS,Flood,300")

    scoped = f"/v1/mcp/dataPlane/workspaces/{ws}/items/{db_id}/kqlEndpoint"
    asyncio.run(session_for(alice, scoped, lambda s: as_alice(s, query_uri, db_name)))
    asyncio.run(session_for(bob, "/v1/mcp/dataPlane/kqlEndpoint", lambda s: as_bob(s, ws, db_id, query_uri)))
    stranger = http("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", {
        "grant_type": "client_credentials", "client_id": CLIENT_ID, "client_secret": CLIENT_SECRET,
        "scope": FABRIC_SCOPE}, form=True)[2]["access_token"]
    asyncio.run(session_for(stranger, "/v1/mcp/dataPlane/kqlEndpoint", lambda s: as_stranger(s, ws, db_id)))

    if failures:
        print(f"\nFAILED: {len(failures)} check(s): {failures}", flush=True)
        sys.exit(1)
    print("\ne2e/mcp-eventhouse: the four Eventhouse MCP tools driven by the unmodified mcp SDK as users against "
          "kustainer, through both endpoints", flush=True)


if __name__ == "__main__":
    main()
