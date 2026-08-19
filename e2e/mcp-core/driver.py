#!/usr/bin/env python3
"""Microsoft's Fabric Core MCP Server, driven by the unmodified Python MCP SDK.

The get-started page documents Streamable HTTP at POST /v1/mcp/core with Entra
Bearer scope https://api.fabric.microsoft.com/.default. This driver is that
conversation: initialize, tools/list (published names), the documented prompts
(list/create workspace, create a lakehouse, search the catalog), then the rest
of the published Core tool list that does not execute notebooks or write
lakehouse tables — Microsoft's own Core MCP Server does not either.

get_knowledge returns plain text, not the {status, body} REST envelope the
other tools wrap.
"""
import asyncio
import json
import os
import ssl
import sys
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

_CTX = ssl.create_default_context()
_CTX.check_hostname = False
_CTX.verify_mode = ssl.CERT_NONE

PUBLISHED = {
    "search_catalog",
    "list_workspaces", "get_workspace", "create_workspace", "update_workspace",
    "delete_workspace",
    "add_workspace_role", "list_workspace_roles", "get_workspace_role",
    "update_workspace_role", "delete_workspace_role",
    "list_items", "get_item", "create_item", "update_item", "delete_item",
    "get_item_definition", "update_item_definition", "bulk_move_items",
    "create_folder", "list_folders", "get_folder", "update_folder",
    "delete_folder", "move_folder",
    "list_capacities", "get_operation_state", "get_operation_result",
    "get_knowledge",
}

failures = []


def check(ok, what, detail=""):
    print(f"  {'PASS' if ok else 'FAIL'}  {what}{(' — ' + detail) if detail else ''}",
          flush=True)
    if not ok:
        failures.append(what)


def fabric_token():
    form = urllib.parse.urlencode({
        "grant_type": "client_credentials",
        "client_id": CLIENT_ID,
        "client_secret": CLIENT_SECRET,
        "scope": "https://api.fabric.microsoft.com/.default",
    }).encode()
    req = urllib.request.Request(f"{ENTRA}/{TENANT}/oauth2/v2.0/token", data=form)
    with urllib.request.urlopen(req, context=_CTX) as r:
        return json.loads(r.read())["access_token"]


def wrap_body(result):
    """MCP tools return the REST envelope as text: {status, body}."""
    if getattr(result, "isError", False):
        raise RuntimeError(f"tool error: {result}")
    text = result.content[0].text
    env = json.loads(text)
    if env.get("status", 200) >= 400:
        raise RuntimeError(f"REST error: {text}")
    return env.get("body")


async def drive(token):
    url = f"{FABRIC}/v1/mcp/core"
    # mcp 2.0 took the per-call `headers`/`auth`/`timeout` kwargs off
    # streamable_http_client and takes a caller-supplied client instead, so the
    # bearer and the emulator's self-signed cert are configured ONCE here rather
    # than per call. Note the client is httpx2, not httpx: mcp 2.0 brought its
    # own stack (httpx2, httpcore2), and passing an httpx.AsyncClient here fails
    # at the type, not at the request.
    async with (
        # verify=False: the emulator serves a self-signed cert, as everywhere
        # else in this harness (see _CTX above).
        httpx2.AsyncClient(headers={"Authorization": "Bearer " + token},
                           verify=False) as http_client,  # noqa: S501
        streamable_http_client(url, http_client=http_client) as streams,
    ):
        read, write = streams[0], streams[1]
        async with ClientSession(read, write) as session:
            await session.initialize()
            tools = await session.list_tools()
            names = {t.name for t in tools.tools}
            missing = PUBLISHED - names
            check(not missing, "tools/list includes the published Core MCP names",
                  f"missing={sorted(missing)}")

            listed = wrap_body(await session.call_tool("list_workspaces", {}))
            value = listed.get("value") if isinstance(listed, dict) else listed
            check(isinstance(value, list), "list_workspaces returns a value array")

            created = wrap_body(await session.call_tool(
                "create_workspace", {"display_name": "Sales Analytics Dev"}))
            wid = created["id"]
            check(created.get("displayName") == "Sales Analytics Dev",
                  "create_workspace Sales Analytics Dev", f"id={wid}")

            got = wrap_body(await session.call_tool("get_workspace", {"workspace_id": wid}))
            check(got.get("id") == wid, "get_workspace returns the created id")

            updated = wrap_body(await session.call_tool(
                "update_workspace", {"workspace_id": wid, "description": "updated"}))
            check(updated.get("description") == "updated", "update_workspace description")

            caps = wrap_body(await session.call_tool("list_capacities", {}))
            cap_value = caps.get("value") if isinstance(caps, dict) else caps
            check(isinstance(cap_value, list) and len(cap_value) > 0,
                  "list_capacities returns at least the emulator capacity")

            folder = wrap_body(await session.call_tool(
                "create_folder", {"workspace_id": wid, "display_name": "src"}))
            fid = folder["id"]
            check(folder.get("displayName") == "src", "create_folder src", f"id={fid}")

            folders = wrap_body(await session.call_tool("list_folders", {"workspace_id": wid}))
            folder_names = [f.get("displayName") for f in (folders.get("value") or folders)]
            check("src" in folder_names, "list_folders includes src")

            fetched = wrap_body(await session.call_tool(
                "get_folder", {"workspace_id": wid, "folder_id": fid}))
            check(fetched.get("id") == fid, "get_folder returns src")

            renamed = wrap_body(await session.call_tool(
                "update_folder",
                {"workspace_id": wid, "folder_id": fid, "display_name": "landing"}))
            check(renamed.get("displayName") == "landing", "update_folder rename to landing")

            item = wrap_body(await session.call_tool("create_item", {
                "workspace_id": wid,
                "display_name": "CustomerData",
                "type": "Lakehouse",
            }))
            iid = item["id"]
            check(item.get("displayName") == "CustomerData" and item.get("type") == "Lakehouse",
                  "create_item CustomerData Lakehouse")

            listed_items = wrap_body(await session.call_tool(
                "list_items", {"workspace_id": wid, "type": "Lakehouse"}))
            item_names = [i.get("displayName") for i in (listed_items.get("value") or listed_items)]
            check("CustomerData" in item_names, "list_items finds the lakehouse")

            got_item = wrap_body(await session.call_tool(
                "get_item", {"workspace_id": wid, "item_id": iid}))
            check(got_item.get("id") == iid, "get_item returns CustomerData")

            found = wrap_body(await session.call_tool("search_catalog", {
                "search": "CustomerData",
                "filter": "Type eq 'Lakehouse'",
            }))
            hits = found.get("value") if isinstance(found, dict) else found
            names_found = [h.get("displayName") for h in hits]
            check("CustomerData" in names_found,
                  "search_catalog finds the lakehouse", f"hits={names_found}")

            moved = wrap_body(await session.call_tool("bulk_move_items", {
                "workspace_id": wid,
                "item_ids": [iid],
                "target_folder_id": fid,
            }))
            moved_value = moved.get("value") if isinstance(moved, dict) else moved
            check(any(m.get("id") == iid and m.get("folderId") == fid for m in moved_value),
                  "bulk_move_items into landing")

            role = wrap_body(await session.call_tool("add_workspace_role", {
                "workspace_id": wid,
                "principal_id": "user-9",
                "role": "Contributor",
            }))
            raid = role["id"]
            check(role.get("role") == "Contributor", "add_workspace_role Contributor")

            roles = wrap_body(await session.call_tool(
                "list_workspace_roles", {"workspace_id": wid}))
            role_ids = [r.get("id") for r in (roles.get("value") or roles)]
            check(raid in role_ids, "list_workspace_roles includes the grant")

            got_role = wrap_body(await session.call_tool(
                "get_workspace_role",
                {"workspace_id": wid, "role_assignment_id": raid}))
            check(got_role.get("role") == "Contributor", "get_workspace_role")

            upd_role = wrap_body(await session.call_tool("update_workspace_role", {
                "workspace_id": wid,
                "role_assignment_id": raid,
                "role": "Viewer",
            }))
            check(upd_role.get("role") == "Viewer", "update_workspace_role to Viewer")

            wrap_body(await session.call_tool("delete_workspace_role", {
                "workspace_id": wid, "role_assignment_id": raid,
            }))
            check(True, "delete_workspace_role")

            wrap_body(await session.call_tool(
                "move_folder", {"workspace_id": wid, "folder_id": fid}))
            check(True, "move_folder to workspace root")

            knowledge = await session.call_tool("get_knowledge", {"item_type": "Lakehouse"})
            ktext = knowledge.content[0].text
            check(not getattr(knowledge, "isError", False) and "Lakehouse" in ktext,
                  "get_knowledge returns plain text for Lakehouse")

            wrap_body(await session.call_tool(
                "delete_item", {"workspace_id": wid, "item_id": iid}))
            wrap_body(await session.call_tool(
                "delete_folder", {"workspace_id": wid, "folder_id": fid}))
            check(True, "delete_item and delete_folder")


def main():
    token = fabric_token()
    asyncio.run(drive(token))
    if failures:
        print(f"\n{len(failures)} check(s) failed", flush=True)
        sys.exit(1)
    print("mcp-core e2e: all checks passed", flush=True)


if __name__ == "__main__":
    main()
