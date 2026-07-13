# 06 — Data model & seed

One SQLite database (`modernc.org/sqlite`, pure Go) holds everything.
`FABRIC_DATA_DIR` empty = in-memory, fresh per run; set = persisted on disk.
Deletes cascade through foreign keys, so removing a workspace takes its items,
definitions, role assignments, git state, OneLake paths, folders, and identity
with it — matching the control plane's documented cascade semantics.

## Tables

```
capacities          (id, displayName, sku, region, state)
workspaces          (id, displayName, description, capacityId, createdAt)
role_assignments    (id, workspace_id ⤳, principal_id, principal_type, role)
items               (id, workspace_id ⤳, type, displayName, description, createdAt)
item_definitions    (item_id ⤳ PK, parts JSON — stored verbatim, round-trips exactly)
folders             (id, workspace_id ⤳, displayName, parent_id (parentFolderId))
operations          (id, kind, createdAt, completeAt, result ref, fail_with — status derived on read)
job_instances       (id, item_id ⤳, jobType, status, start/end times)
pipeline_runs       (job_id ⤳ PK, status, activity_runs JSON)
notebook_runs       (job_id ⤳ PK, status, run JSON — {status, exitValue, cells})
shortcuts           (item_id ⤳ + path + name PK, target_workspace, target_item, target_path, created_at)
connections         (id, displayName, connectivityType, details)
git_connections     (workspace_id ⤳ PK, provider details, connection_id, branch, sync state)
git_remote_items    (remote_key, branch, item_type, display_name → logical id, definition)
git_remote_heads    (remote_key, branch → head)
onelake_paths       (item_id ⤳ + rel_path PK, workspace_id ⤳, is_directory, content)
workspace_identities(workspace_id ⤳ PK, application_id, service_principal_id)

⤳ = FK with ON DELETE CASCADE
```

Three notable choices:

- **Item definitions are opaque.** `parts` (the `.platform` + base64 payload
  format) are stored verbatim, never parsed — which is why `getDefinition`
  returns byte-for-byte what `updateDefinition` or a git sync wrote, and why
  `fabric-cicd` round-trips cleanly.
- **The "git remote" is local.** `git_remote_items`/`git_remote_heads` model a
  per-branch remote keyed by provider/org/repo, preserving logical ids across
  commits — no real GitHub/AzDO involved.
- **OneLake blobs live in the same DB** (`onelake_paths.content`), so the data
  plane and control plane can never disagree about what exists.

## Identity of the caller

There are no user rows: **principals live in entra-emulator** (or your real
tenant). fabric-emulator sees whatever `oid`/`appid` a validated token
carries and maps it to a role via `role_assignments` — the workspace creator
gets `Admin` automatically.

## The seed

Deterministic, idempotent, and minimal — one row:

| | |
|---|---|
| Capacity id | `eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee` |
| Shape | `{ displayName: "Emulator Capacity", sku: "F64", region: "local", state: "Active" }` |

Every workspace created without an explicit `capacityId` is auto-assigned to
it (tools like `fabric-cicd` refuse capacity-less workspaces). Everything else
— workspaces, items, principals — starts empty: tokens from entra-emulator's
seeded apps work immediately, and the first authenticated caller to create a
workspace becomes its Admin.

## State enums

- `workspace_identities` state lives entra-side
  (`Active/Provisioning/Failed/Deprovisioning`) — see the
  [identity handshake](09-identity-handshake.md).
- `operations.status`: `NotStarted/Running/Succeeded/Failed`;
  `job_instances.status`: `NotStarted/InProgress/Completed/Failed/Cancelled`.
