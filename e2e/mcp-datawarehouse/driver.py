#!/usr/bin/env python3
"""Microsoft's Fabric Data Warehouse MCP server, driven by the unmodified Python
MCP SDK, as users, against a real SQL Server.

Microsoft's page documents a global endpoint (POST /v1/mcp/dataPlane/sqlEndpoint)
and one item-scoped to a warehouse, one tool that runs T-SQL "using the signed-in
user's identity", and no separate schema tools: discovery is a query against
INFORMATION_SCHEMA. Microsoft's skills call the tool
execute_query(workspaceId, itemId, query) and read its result as CSV. This driver:

  1. signs in entra-emulator's seeded users with the password grant;
  2. as Alice (workspace Admin), creates a Warehouse and a Lakehouse, and makes
     Bob a workspace Viewer;
  3. as Alice on the global endpoint, creates a table, a row-level security
     policy keyed on the connected user, and finds the table through
     INFORMATION_SCHEMA, as Microsoft's page says an agent does;
  4. as Bob on the endpoint scoped to that warehouse, reads only his row, and
     is refused a write;
  5. reaches the lakehouse through its SQL analytics endpoint id, where data is
     read-only, and is told to use that id when it names the lakehouse itself.
"""
import asyncio
import base64
import csv
import io
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
    data, hdrs = None, {}
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
        with urllib.request.urlopen(req, context=_CTX, timeout=60) as r:
            raw = r.read()
            return r.status, {k.lower(): v for k, v in r.headers.items()}, json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        raw = e.read()
        return e.code, {k.lower(): v for k, v in e.headers.items()}, json.loads(raw) if raw else None


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


def create_item(tok, ws, body):
    status, hdrs, out = http("POST", f"{FABRIC}/v1/workspaces/{ws}/items", body, token=tok)
    if status == 201:
        return out["id"]
    if status != 202:
        raise SystemExit(f"create {body['displayName']}: {status} {out}")
    opid = hdrs.get("x-ms-operation-id")
    for _ in range(100):
        state = http("GET", f"{FABRIC}/v1/operations/{opid}", token=tok)[2]
        if state.get("status") == "Succeeded":
            return http("GET", f"{FABRIC}/v1/operations/{opid}/result", token=tok)[2]["id"]
        time.sleep(0.1)
    raise SystemExit(f"create {body['displayName']} did not complete")


def rows_of(result):
    """The CSV block as rows, header first; the metadata block is the last one."""
    return list(csv.reader(io.StringIO(result.content[0].text, newline="")))


def meta_of(result):
    return result.content[-1].text


async def session_for(tok, path, fn):
    async with (
        httpx2.AsyncClient(headers={"Authorization": "Bearer " + tok}) as http_client,
        streamable_http_client(f"{FABRIC}{path}", http_client=http_client) as streams,
        ClientSession(streams[0], streams[1]) as session,
    ):
        await session.initialize()
        return await fn(session)


async def as_alice(session, ws, wh, alice_oid):
    tools = (await session.list_tools()).tools
    check([t.name for t in tools] == ["execute_query"], "tools/list is the one T-SQL tool",
          str([t.name for t in tools]))
    required = set(tools[0].input_schema.get("required", []))
    check(required == {"workspaceId", "itemId", "query"}, "the global endpoint asks which item", str(required))

    async def run(query):
        r = await session.call_tool("execute_query", {"workspaceId": ws, "itemId": wh, "query": query})
        if r.is_error:
            raise RuntimeError(f"{query.split()[0]}: {r.content[0].text}")
        return r

    for stmt in (
        "CREATE TABLE dbo.orders (owner_oid sysname, region nvarchar(20), amount decimal(10,2))",
        f"INSERT INTO dbo.orders VALUES (N'{alice_oid}', N'West', 120.50), (N'{BOB_OID}', N'East', 80.25), "
        f"(N'{BOB_OID}', N'East, North', 10.00)",
        "CREATE SCHEMA sec",
        "CREATE FUNCTION sec.fn_mine(@owner sysname) RETURNS TABLE WITH SCHEMABINDING "
        "AS RETURN SELECT 1 AS ok WHERE @owner = USER_NAME()",
        "CREATE SECURITY POLICY sec.mine ADD FILTER PREDICATE sec.fn_mine(owner_oid) ON dbo.orders WITH (STATE = ON)",
    ):
        r = await run(stmt)
        check(len(r.content) == 1 and "no result set" in meta_of(r), f"Alice: {stmt.split('(')[0].strip()}",
              meta_of(r))

    found = rows_of(await run(
        "SELECT TABLE_SCHEMA, TABLE_NAME FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_NAME = 'orders'"))
    check(found == [["TABLE_SCHEMA", "TABLE_NAME"], ["dbo", "orders"]],
          "INFORMATION_SCHEMA finds the table, as Microsoft's page says an agent discovers it", str(found))

    mine = rows_of(await run("SELECT region, amount FROM dbo.orders"))
    check(mine == [["region", "amount"], ["West", "120.50"]], "Alice's own row only", str(mine))

    last = rows_of(await run("SELECT 1 AS first; SELECT COUNT(*) AS n FROM dbo.orders"))
    check(last == [["n"], ["1"]], "only the last result set comes back", str(last))

    alias = await session.call_tool("executeSQL", {"workspaceId": ws, "itemId": wh, "query": "SELECT 1 AS one"})
    check(not alias.is_error and rows_of(alias) == [["one"], ["1"]],
          "executeSQL, the Learn page's name, is the same tool")


async def as_bob(session, wh):
    tools = (await session.list_tools()).tools
    check(set(tools[0].input_schema.get("required", [])) == {"query"},
          "the item-scoped endpoint takes its warehouse from the URL")
    r = await session.call_tool("execute_query", {"query": "SELECT region, amount FROM dbo.orders ORDER BY amount"})
    got = rows_of(r)
    check(not r.is_error and got == [["region", "amount"], ["East, North", "10.00"], ["East", "80.25"]],
          "Bob's rows only, and a comma inside a value survives the CSV", str(got))
    w = await session.call_tool("execute_query", {"query": "INSERT INTO dbo.orders VALUES (N'x', N'x', 1)"})
    check(w.is_error and "this session is read-only" in w.content[0].text, "Bob's Viewer session is read-only",
          w.content[0].text[:160])
    other = await session.call_tool("execute_query", {"itemId": "00000000-0000-0000-0000-000000000000",
                                                     "query": "SELECT 1"})
    check(other.is_error and f"bound to item {wh}" in other.content[0].text,
          "the scoped endpoint refuses another item", other.content[0].text[:160])


async def the_lakehouse(session, ws, lh, endpoint):
    read = await session.call_tool("execute_query", {"workspaceId": ws, "itemId": endpoint,
                                                     "query": "SELECT DB_NAME() AS db"})
    check(not read.is_error and rows_of(read) == [["db"], [lh]],
          "a SQL analytics endpoint id runs on its lakehouse's database", str(rows_of(read)) if not read.is_error else
          read.content[0].text)
    write = await session.call_tool("execute_query", {"workspaceId": ws, "itemId": endpoint,
                                                      "query": "CREATE TABLE dbo.t (a int)"})
    check(write.is_error and "SQL analytics endpoint is read-only" in write.content[0].text,
          "the endpoint's data is read-only",
          write.content[0].text[:160])
    named = await session.call_tool("execute_query", {"workspaceId": ws, "itemId": lh, "query": "SELECT 1"})
    check(named.is_error and "sqlEndpointProperties.id" in named.content[0].text,
          "naming the lakehouse itself is refused with the id to use", named.content[0].text[:160])


def main():
    alice, bob = user_token(ALICE), user_token(BOB)
    status, _, out = http("POST", f"{FABRIC}/v1/workspaces", {"displayName": "dw-mcp-e2e"}, token=alice)
    if status not in (200, 201):
        raise SystemExit(f"create workspace: {status} {out}")
    ws = out["id"]
    status, _, out = http("POST", f"{FABRIC}/v1/workspaces/{ws}/roleAssignments",
                          {"principal": {"id": BOB_OID, "type": "User"}, "role": "Viewer"}, token=alice)
    if status not in (200, 201):
        raise SystemExit(f"make Bob a Viewer: {status} {out}")
    wh = create_item(alice, ws, {"displayName": "SalesDW", "type": "Warehouse"})
    lh = create_item(alice, ws, {"displayName": "Bronze", "type": "Lakehouse"})
    endpoint = http("GET", f"{FABRIC}/v1/workspaces/{ws}/lakehouses/{lh}", token=alice)[2]["properties"][
        "sqlEndpointProperties"]["id"]
    print(f"==> workspace={ws} warehouse={wh} lakehouse={lh} endpoint={endpoint}", flush=True)

    asyncio.run(session_for(alice, "/v1/mcp/dataPlane/sqlEndpoint", lambda s: as_alice(s, ws, wh, oid_of(alice))))
    asyncio.run(session_for(bob, f"/v1/mcp/dataPlane/workspaces/{ws}/items/{wh}/sqlEndpoint",
                            lambda s: as_bob(s, wh)))
    asyncio.run(session_for(alice, "/v1/mcp/dataPlane/sqlEndpoint", lambda s: the_lakehouse(s, ws, lh, endpoint)))

    if failures:
        print(f"\nFAILED: {len(failures)} check(s): {failures}", flush=True)
        sys.exit(1)
    print("\ne2e/mcp-datawarehouse: execute_query driven by the unmodified mcp SDK as users on a real SQL "
          "Server; row-level security per caller; Viewer and analytics endpoint read-only", flush=True)


if __name__ == "__main__":
    main()
