# 08 — OneLake data plane

The `onelake.dfs.fabric.microsoft.com` surface: an ADLS-Gen2-shaped data
plane over the same store as the [control plane](07-control-plane-api.md),
accepting `Storage`-audience Entra tokens. Grounding: `onelake-access-api.md`
and `onelake-api-parity.md` in the pinned `fabric-docs` commit
(see [03-architecture.md](03-architecture.md), "Version grounding").

## The DFS surface (shipped, P3)

An **ADLS-Gen2 / Blob** subset (DFS endpoint), `Storage`-audience token. The
**filesystem is the workspace** (account name is always `onelake`), so listing
happens at the workspace level:

- `PUT  /{workspace}/{item}.{type}/{path}?resource=file|directory` — create
- `PATCH …?action=append` + `?action=flush` — write
- `GET  /{workspace}/{item}.{type}/{path}` — read
- `GET  /{workspace}?resource=filesystem&recursive=false[&directory={item}.{type}/Files]` — list
- `DELETE …`

**Managed-folder rules** (`onelake-api-parity.md` — core fidelity, not
optional): ADLS/Blob APIs can **never create, rename, or delete workspaces or
items** — only `HEAD` is allowed at the workspace (container) and tenant
(account) level. An item's top-level folder (`/MyLakehouse.lakehouse`) and its
first level (`/Files`, `/Tables`) are Fabric-managed: protected from
create/delete/rename; full CRUD only *within* them. Disallowed query parameters
(e.g. `action=setAccessControl`) reject the request; disallowed headers (e.g.
`x-ms-owner`) are ignored and echoed back in `x-ms-rejected-headers`.
Permission response headers are canned: `x-ms-owner`/`x-ms-group` =
`$superuser`, `x-ms-permissions` = `---------`.

Enough for shortcut / trusted-workspace-access smoke tests. GUID and
name-addressing both resolve to the same item.

## Shortcuts (planned)

Wire shapes are REST-reference-only (`/rest/api/fabric/core/onelake-shortcuts`;
fabric-docs covers shortcut creation portal-side). A shortcut is a **symlink in
OneLake**: a named entry inside an item's managed folders whose reads resolve
into another location. Scope: **OneLake-to-OneLake targets only** — external
targets (ADLS Gen2, S3, Dataverse, …) need real cloud credentials, which is
exactly what an offline emulator cannot honor; they 501 with a clear message.

| Method + path | Notes |
|---|---|
| `POST /v1/workspaces/{wid}/items/{iid}/shortcuts` | create → 201 |
| `GET  /v1/workspaces/{wid}/items/{iid}/shortcuts` | list *sync* |
| `GET  /v1/workspaces/{wid}/items/{iid}/shortcuts/{path}/{name}` | get *sync* |
| `DELETE /v1/workspaces/{wid}/items/{iid}/shortcuts/{path}/{name}` | delete (removes the link, never the target) |

**Create body** (the OneLake target):

```json
{
  "path": "Files",
  "name": "linked-data",
  "target": { "oneLake": { "workspaceId": "…", "itemId": "…", "path": "Files/raw" } }
}
```

- **Data-plane resolution:** on the DFS surface, `/{ws}/{item}/Files/linked-data/…`
  resolves reads/lists through to the target item's `Files/raw/…`. Writes
  through shortcuts follow the target's RBAC (the caller needs a role on the
  *target* workspace — this is the trusted-workspace-access smoke path).
- **Listing:** shortcut entries appear in filesystem listings as directories
  with `isShortcut: true` metadata on the shortcut API (plain directories on
  the DFS listing, as in real OneLake).
- **Integrity:** deleting the target item leaves a dangling shortcut whose
  resolution 404s (matching real behavior); deleting the shortcut never
  touches target data. Cycles are rejected at create (`400 InvalidTarget`).
- **Storage:** a `shortcuts` table (`item_id, path, name, target_ws, target_item,
  target_path`) — no data is copied, resolution happens per request.
