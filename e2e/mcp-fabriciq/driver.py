#!/usr/bin/env python3
"""Microsoft's Fabric IQ MCP, driven by the unmodified Python MCP SDK, as users.

Microsoft's page documents Streamable HTTP at POST /v1/mcp/fabriciq, the
X-Variants selector `Fabric.Routing.FabricIQ.V1`, delegated Entra tokens only,
and six read-only tools. This driver:

  1. signs in entra-emulator's seeded users with the password grant (its own
     test users and test password, docs/06 of that repository);
  2. as Alice, creates a workspace, the golden retail semantic model with a
     row-level security role admitting Bob to the West territory, and a PBIR
     report bound to the model by id, and makes Bob a workspace Viewer;
  3. as Alice (Admin, so unrestricted) drives all six tools the way Microsoft's
     Fabric IQ skill does: discover, resolve a browser URL, read the report and
     the schema, look up a value, run DAX;
  4. as Bob, runs the same DAX and value search and gets only the West rows;
  5. shows the daemon app's app-only token refused, and an unknown X-Variants
     refused, before any tool runs.
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
VARIANT = "Fabric.Routing.FabricIQ.V1"

# entra-emulator's seeded users (its docs/06-data-model-and-seed.md).
ALICE = "alice@entraemulator.dev"
BOB = "bob@entraemulator.dev"
BOB_OID = "0d4ba1f9-cab1-4200-b516-d4cb8b340930"
SEED_PASSWORD = "Password1!"

FIX = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                   "semantic-model", "fixtures")

TOOLS = {"DiscoverArtifacts", "ResolveFabricItem", "GetReportMetadata",
         "GetSemanticModelSchema", "ValueSearch", "ExecuteQuery"}

UNITS_BY_TERRITORY = ("EVALUATE SUMMARIZECOLUMNS('Store'[Territory], \"Units\", [TotalUnits]) "
                      "ORDER BY [Units] DESC")

_CTX = ssl.create_default_context()
_CTX.check_hostname = False
_CTX.verify_mode = ssl.CERT_NONE

failures = []


def check(ok, what, detail=""):
    print(f"  {'PASS' if ok else 'FAIL'}  {what}{(' — ' + detail) if detail else ''}", flush=True)
    if not ok:
        failures.append(what)


def http(method, url, body=None, token=None, headers=None, form=False):
    data, hdrs = None, dict(headers or {})
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
        with urllib.request.urlopen(req, context=_CTX, timeout=30) as r:
            raw = r.read()
            return r.status, {k.lower(): v for k, v in r.headers.items()}, json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        raw = e.read()
        return e.code, {k.lower(): v for k, v in e.headers.items()}, json.loads(raw) if raw else None


def token(form):
    status, _, body = http("POST", f"{ENTRA}/{TENANT}/oauth2/v2.0/token", form, form=True)
    if status != 200:
        raise SystemExit(f"token request failed: {status} {body}")
    return body["access_token"]


def user_token(upn):
    return token({"grant_type": "password", "client_id": CLIENT_ID, "client_secret": CLIENT_SECRET,
                  "username": upn, "password": SEED_PASSWORD, "scope": FABRIC_SCOPE})


def app_token():
    return token({"grant_type": "client_credentials", "client_id": CLIENT_ID,
                  "client_secret": CLIENT_SECRET, "scope": FABRIC_SCOPE})


def claims(jwt):
    payload = jwt.split(".")[1]
    return json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))


def b64(data):
    return base64.b64encode(data if isinstance(data, bytes) else data.encode()).decode()


def create_item(tok, ws, body):
    status, hdrs, out = http("POST", f"{FABRIC}/v1/workspaces/{ws}/items", body, token=tok)
    if status == 201:
        return out["id"]
    if status != 202:
        raise SystemExit(f"create {body['displayName']}: {status} {out}")
    opid = hdrs.get("x-ms-operation-id")
    state = None
    for _ in range(100):
        state = http("GET", f"{FABRIC}/v1/operations/{opid}", token=tok)[2]
        if state.get("status") == "Succeeded":
            return http("GET", f"{FABRIC}/v1/operations/{opid}/result", token=tok)[2]["id"]
        if state.get("status") in ("Failed", "Cancelled"):
            break
        time.sleep(0.1)
    raise SystemExit(f"create {body['displayName']} did not complete: {state}")


def secured_model_bim():
    """retail.bim with a role that admits Bob to the West territory only."""
    bim = open(os.path.join(FIX, "retail.bim"), encoding="utf-8").read()
    role = ('"roles":[{"name":"West","modelPermission":"read",'
            f'"members":[{{"memberName":"{BOB}","memberId":"{BOB_OID}","identityProvider":"AzureAD"}}],'
            '"tablePermissions":[{"name":"Store","filterExpression":"\'Store\'[Territory] = \\"West\\""}]}],')
    i = bim.index('"tables"')
    return bim[:i] + role + bim[i:]


def report_parts(model_id):
    field = lambda kind, entity, prop: {kind: {"Expression": {"SourceRef": {"Entity": entity}}, "Property": prop}}  # noqa: E731
    visual = {"name": "v1", "visual": {
        "visualType": "clusteredBarChart",
        "query": {"queryState": {
            "Category": {"projections": [{"field": field("Column", "Store", "Territory")}]},
            "Y": {"projections": [{"field": field("Measure", "Sales", "TotalUnits")}]}}},
        "visualContainerObjects": {"title": [{"properties": {"text": {"expr": {"Literal": {"Value": "'Units by territory'"}}}}}]}}}
    pbir = {"version": "4.0", "datasetReference": {"byConnection": {"connectionString":
            f"Data Source=powerbi://api.powerbi.com/v1.0/myorg/iq;Initial Catalog=Retail;semanticmodelid={model_id}"}}}
    part = lambda path, doc: {"path": path, "payloadType": "InlineBase64", "payload": b64(json.dumps(doc))}  # noqa: E731
    return [part("definition.pbir", pbir),
            part("definition/report.json", {"filterConfig": {"filters": []}}),
            part("definition/pages/pages.json", {"pageOrder": ["p1"]}),
            part("definition/pages/p1/page.json", {"name": "p1", "displayName": "Territories"}),
            part("definition/pages/p1/visuals/v1/visual.json", visual)]


def text_of(result):
    return result.content[0].text


def doc_of(result, what):
    if result.is_error:
        raise RuntimeError(f"{what}: tool error {text_of(result)}")
    return json.loads(text_of(result))


async def session_for(tok, fn):
    async with (
        httpx2.AsyncClient(headers={"Authorization": "Bearer " + tok, "X-Variants": VARIANT},
                           verify=False) as http_client,  # noqa: S501
        streamable_http_client(f"{FABRIC}/v1/mcp/fabriciq", http_client=http_client) as streams,
        ClientSession(streams[0], streams[1]) as session,
    ):
        init = await session.initialize()
        return init, await fn(session)


async def as_alice(session, ws, model, report):
    tools = await session.list_tools()
    names = {t.name for t in tools.tools}
    check(names == TOOLS, "tools/list is exactly Microsoft's six Fabric IQ tools", f"got {sorted(names)}")
    check(all(t.annotations and t.annotations.read_only_hint for t in tools.tools),
          "every tool is annotated read-only")

    found = doc_of(await session.call_tool("DiscoverArtifacts", {"searchQuery": "retail"}), "DiscoverArtifacts")
    arts = {a["ArtifactId"]: a for a in found["Artifacts"]}
    check(set(arts) == {report, model}, "DiscoverArtifacts finds the report and the model", json.dumps(found)[:200])
    # "retail" is the model's whole name and only part of the report's, so the
    # model ranks first: an exact name outranks a partial one.
    check(found["Artifacts"][0]["ArtifactId"] == model, "an exact name ranks first")
    check(arts.get(report, {}).get("SemanticModelId") == model, "the discovered report names its semantic model")

    url = f"https://app.powerbi.com/groups/{ws}/reports/{report}/ReportSection?experience=power-bi"
    resolved = doc_of(await session.call_tool("ResolveFabricItem", {"fabricItemId": url}), "ResolveFabricItem")
    check(resolved["itemType"] == "Report" and resolved["fabricItemId"] == report,
          "ResolveFabricItem maps a browser URL to the report", json.dumps(resolved)[:200])

    meta = doc_of(await session.call_tool("GetReportMetadata", {"reportObjectId": report}), "GetReportMetadata")
    page = meta["ReportMetadata"]["Pages"][0]
    check(meta["semanticModel"] == model, "GetReportMetadata's semanticModel is the bound model's id")
    check(page["Title"] == "Territories" and page["Visuals"][0]["Title"] == "Units by territory",
          "GetReportMetadata reads the page and the visual's title")
    fields = [f["Reference"] for f in page["Visuals"][0]["Fields"]]
    check(fields == ["'Store'[Territory]", "'Sales'[TotalUnits]"], "the visual's bound fields", str(fields))

    schema = doc_of(await session.call_tool("GetSemanticModelSchema", {"artifactId": model}), "GetSemanticModelSchema")
    tables = {t["Name"] for t in schema["schema"]["Tables"]}
    check({"Store", "Sales", "Time"} <= tables, "GetSemanticModelSchema lists the model's tables", str(tables))
    rels = schema["schema"]["ActiveRelationships"]
    check({"PK": "'Store'[StoreId]", "FK": "'Sales'[StoreId]"} in rels, "relationships as PK/FK", json.dumps(rels))

    queried = doc_of(await session.call_tool("GetSemanticModelSchema", {"artifactId": model, "queries": [
        "schema.Tables[].Measures[?regex_match(to_string(@), 'units delta')].Name | []"]}), "schema queries")
    # to_string(@) includes each measure's expression, so a measure that USES
    # [Units Delta] matches too, as it would in the skill's own example.
    check("Units Delta" in queried["Results"][0]["Result"], "a JMESPath query with regex_match, as the skill writes it",
          json.dumps(queried))

    values = doc_of(await session.call_tool("ValueSearch", {"artifactId": model, "searchTerms": ["west"]}), "ValueSearch")
    m = values["Results"][0]["Matches"]
    check(m and m[0]["Value"] == "West" and m[0]["ColumnReference"] == "'Store'[Territory]",
          "ValueSearch returns the model's own spelling and column", json.dumps(m)[:200])

    rows = doc_of(await session.call_tool("ExecuteQuery", {"artifactId": model, "daxQueries": [UNITS_BY_TERRITORY]}),
                  "ExecuteQuery")["Results"][0]["Rows"]
    units = [r["[Units]"] for r in rows]
    check(len(rows) == 3 and units == sorted(units, reverse=True),
          "ExecuteQuery runs DAX with ORDER BY: three territories, largest first", json.dumps(rows))


async def as_bob(session, model):
    rows = doc_of(await session.call_tool("ExecuteQuery", {"artifactId": model, "daxQueries": [UNITS_BY_TERRITORY]}),
                  "ExecuteQuery as Bob")["Results"][0]["Rows"]
    check([r["Store[Territory]"] for r in rows] == ["West"],
          "Bob's role admits the West territory only: the same query, one row", json.dumps(rows))
    east = doc_of(await session.call_tool("ValueSearch", {"artifactId": model, "searchTerms": ["East"]}),
                  "ValueSearch as Bob")
    check(east["Results"][0]["Matches"] == [], "ValueSearch cannot see the rows Bob's role hides")


def main():
    alice, bob = user_token(ALICE), user_token(BOB)
    check(claims(alice).get("idtyp") != "app" and claims(alice).get("oid"), "Alice's token is delegated (has an oid)")

    status, _, out = http("POST", f"{FABRIC}/v1/workspaces", {"displayName": "fabric-iq-e2e"}, token=alice)
    if status not in (200, 201):
        raise SystemExit(f"create workspace as Alice: {status} {out}")
    ws = out["id"]
    status, _, out = http("POST", f"{FABRIC}/v1/workspaces/{ws}/roleAssignments",
                          {"principal": {"id": BOB_OID, "type": "User"}, "role": "Viewer"}, token=alice)
    if status not in (200, 201):
        raise SystemExit(f"make Bob a Viewer: {status} {out}")
    model = create_item(alice, ws, {"displayName": "Retail", "type": "SemanticModel", "definition": {"parts": [
        {"path": "model.bim", "payloadType": "InlineBase64", "payload": b64(secured_model_bim())},
        {"path": "data.json", "payloadType": "InlineBase64",
         "payload": b64(open(os.path.join(FIX, "seed_data.json"), "rb").read())}]}})
    report = create_item(alice, ws, {"displayName": "Retail Sales", "type": "Report",
                                     "definition": {"parts": report_parts(model)}})
    print(f"==> workspace={ws} model={model} report={report}", flush=True)

    init, _ = asyncio.run(session_for(alice, lambda s: as_alice(s, ws, model, report)))
    check(init.server_info.name == "fabric-iq", "initialize names the fabric-iq server", init.server_info.name)
    asyncio.run(session_for(bob, lambda s: as_bob(s, model)))

    ping = {"jsonrpc": "2.0", "id": 1, "method": "ping"}
    status, _, out = http("POST", f"{FABRIC}/v1/mcp/fabriciq", ping, token=app_token())
    check(status == 403 and (out or {}).get("errorCode") == "ServicePrincipalNotSupported",
          "an app-only token is refused before any tool runs", f"{status} {out}")
    status, _, out = http("POST", f"{FABRIC}/v1/mcp/fabriciq", ping, token=alice,
                          headers={"X-Variants": "Fabric.Routing.FabricIQ.V2"})
    check(status == 400 and (out or {}).get("errorCode") == "UnsupportedVariant",
          "an unknown X-Variants is refused", f"{status} {out}")

    if failures:
        print(f"\nFAILED: {len(failures)} check(s): {failures}", flush=True)
        sys.exit(1)
    print("\ne2e/mcp-fabriciq: all six Fabric IQ tools driven by the unmodified mcp SDK as users; "
          "row-level security applied per caller; app-only tokens and unknown variants refused", flush=True)


if __name__ == "__main__":
    main()
