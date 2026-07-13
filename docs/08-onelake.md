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

## Shortcuts (shipped)

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

- **Read-only resolution:** shortcuts resolve on **reads only** (GET/HEAD). On
  the DFS surface, `/{ws}/{item}/Files/linked-data/…` resolves through to the
  target item's `Files/raw/…` when the direct path is absent. Resolution is
  authorized against the **target** workspace's RBAC (the caller needs
  Contributor/ReadAll there — the trusted-workspace-access smoke path).
- **Not in listings:** the DFS list handler enumerates only real stored paths,
  so shortcut entries do **not** appear in directory listings.
- **Writes don't follow:** PUT/PATCH/DELETE on a shortcut path write the source
  item, not the target — resolution is a read-side concern only.
- **API shape:** the shortcut endpoints return `{ path, name, target }` only —
  there is no `isShortcut` field.
- **Integrity:** creating a shortcut to a missing item is rejected
  (`400 TargetNotFound`); deleting the target *after* creation leaves a dangling
  shortcut whose resolution 404s (matching real behavior); deleting the shortcut
  never touches target data. Self-referential cycles are rejected at create
  (`400 InvalidTarget`).
- **Storage:** a `shortcuts` table (`item_id, path, name, target_ws, target_item,
  target_path`) — no data is copied, resolution happens per request.

## The Blob surface (Delta commits)

OneLake serves the same store over a **Blob dialect**
(`onelake.blob.fabric.microsoft.com`) alongside DFS (`onelake-api-parity.md`).
The Blob dialect is what Rust `object_store` — and therefore **delta-rs** —
speaks, so this is the surface delta writers actually hit. Two addressings
reach it:

- **Host** `onelake.blob.…` → `/{workspace}/{blob…}`
- **Any host** (endpoint override, azurite-style) → `/onelake/{workspace}/{blob…}`
  — the `/onelake` account prefix is stripped by the router; the account name is
  always the literal `onelake`.

Same managed-folder rules as DFS: PUT/DELETE at the workspace, item root, or its
first managed level are rejected (`409`); blobs live only *within* the managed
first level.

| Operation | Notes |
|---|---|
| `PUT …/{path}` | Put Blob (whole-blob write) |
| `PUT …/{path}?comp=block&blockid=…` | Put Block — stage an uncommitted block (base64 id) |
| `PUT …/{path}?comp=blocklist` | Put Block List — commit staged blocks in order (XML body) |
| `PUT …/{path}` + `x-ms-copy-source` | Copy Blob |
| `GET/HEAD …/{path}` | read (Range supported) |
| `DELETE …/{path}` | delete |
| `GET /{workspace}?comp=list` | List Blobs (XML) |

**Delta commit primitive.** Put Blob with **`If-None-Match: *`** is a
**put-if-absent**: it succeeds only if the blob does not yet exist, else `409
BlobAlreadyExists`. This is exactly how Delta Lake commits a new `_delta_log`
entry atomically — the conditional create is what makes concurrent writers race
safely for a version number. delta-rs / `object_store` rely on it; the emulator
honors it against the same store the DFS surface reads.
