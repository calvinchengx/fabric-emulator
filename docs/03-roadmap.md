# 03 — Roadmap

Scope chosen: **full** — control plane through the OneLake data plane, matching
entra-emulator's surfaces (portal + Starlight docs + distribution), composed via
docker-compose. Each phase is independently useful and CI-verified (real-SDK
e2e, like entra-emulator's SDK matrix).

## P0 — the spine (token acceptance + workspaces + items + RBAC + LRO)

The minimum that lets someone test **SP → Fabric client-credentials** automation.

- [x] Token acceptance: validate Bearer against entra-emulator JWKS/issuer;
      audience set; `oid`/`appid` extraction. (`--entra-issuer`, `--entra-jwks-url`)
- [x] Store + migrations (`workspace`, `item`, `role_assignment`, `operation`).
- [x] **LRO engine** on the controllable clock (`202`/`x-ms-operation-id`/
      `Location`/`Retry-After`, `GET /operations/{id}` + `/result`).
- [x] Workspaces CRUD. (`assignToCapacity` deferred — no capacity model yet.)
- [x] Generic items CRUD (create-with-definition → 202 LRO).
- [x] RBAC: role assignments CRUD + enforcement (Admin/Member/Contributor/Viewer;
      creator becomes Admin; Member grants ≤ Member).
- [x] Fault injection + clock control (`/_emulator/clock`, `/_emulator/faults`).
- [x] Health; Docker image; docker-compose with entra-emulator.
- [x] e2e: in-process entra-emulator mints a real client-credentials token for
      the Fabric audience; full workspace/RBAC/item/LRO flow over HTTP.

## P1 — CI/CD (the primary draw)

Makes `fabric-cicd`, git integration, and deployment pipelines run offline.

- [x] Item **definitions**: `getDefinition` (200) / `updateDefinition` (202 LRO),
      parts round-trip verbatim.
- [x] Typed item aliases (12 collections: notebooks, lakehouses, warehouses,
      dataPipelines, semanticModels, reports, environments, eventhouses,
      kqlDatabases, sparkJobDefinitions, mirroredDatabases, eventstreams).
- [x] **Connections**: `GET/POST /v1/connections` — git `connect` with a
      service principal requires a `connectionId` (SPs may not use `Automatic`).
- [x] **Git integration**: connect / initializeConnection / status / commitToGit /
      updateFromGit / disconnect / myGitCredentials, backed by a local
      per-branch definition store (logical ids preserved across commits;
      updateFromGit mirrors: creates, replaces definitions, deletes stale).
- [x] Jobs: `jobs/instances?jobType=` trigger (202 + Location) + clock-derived
      `NotStarted→InProgress→Completed/Failed` + cancel (`Cancelled`).
- [x] e2e: two-workspace git round-trip over HTTP (commit from one, update
      into another, definitions intact); job lifecycle on the frozen clock.
- [x] e2e: the **real `fabric-cicd` Python tool** (v1.2.x) publishes into the
      emulator — `e2e/fabric-cicd/run.sh` (self-contained: both emulators +
      venv + driver). Works unmodified via its own `FABRIC_API_ROOT_URL` /
      `DEFAULT_API_ROOT_URL` overrides + in-process DNS pin (our TLS cert
      covers `api.fabric.microsoft.com`). Driving it surfaced and fixed real
      gaps: `/v1/workspaces/{id}/folders` (now implemented), `description`
      always present on item wire shapes, result-less LROs must not advertise
      a result Location, and fabric-cicd refuses workspaces with no
      `capacityId`. Remaining: wire into CI once the GitHub remote exists.

## P2 — the identity handshake (deepest entra integration)

The "works seamlessly with entra-emulator" payoff. Its dependency —
entra-emulator roadmap #16 — **has already shipped**: the workspace-identity
object (`internal/store/fabric.go`, states `Active/Provisioning/Failed/
Deprovisioning`, name-follows-workspace, cascade delete), admin CRUD at
`/admin/api/workspace-identities`, internal token minting at
`GET /fabric/workspaceidentities/{id}/token`, and acceptance of both Fabric
audiences. P2 can start any time; it consumes those endpoints over HTTP.

- [x] Workspace-identity lifecycle: `POST /v1/workspaces/{id}/provisionIdentity`
      / `deprovisionIdentity` (202 LRO) drive entra's admin API over HTTP
      (`internal/entra` client, origin derived from the issuer). Rename
      follows the workspace; workspace delete cascades the identity; the
      identity appears as `workspaceIdentity{applicationId,servicePrincipalId}`
      on the workspace shape.
- [x] The provisioned identity's SP is granted **Admin on its workspace**, so
      tokens entra mints for it (`GET /fabric/workspaceidentities/{id}/token`
      — customer never holds a credential) pass RBAC back here. Deprovision
      revokes the grant.
- [x] Audit event parity: entra-side — its token mint emits
      `Retrieved Fabric Identity Token for Workspace` (covered by its tests).
- [x] e2e: provision → entra mints for the identity → the identity's token
      reads its workspace and creates items in fabric-emulator; rename-follows
      verified in entra; deprovision revokes; workspace delete cascades.

## P3 — OneLake data plane

- [x] `onelake.` host mux (Host-routed like real Fabric): ADLS-Gen2 subset —
      PUT create file/directory, PATCH append/flush (position-checked), GET
      read, HEAD properties, filesystem listing (`?resource=filesystem`,
      `directory=`, `recursive=`, non-recursive collapses to first-level
      dirs), DELETE (directories take their subtree).
- [x] `Storage`-audience token acceptance (separate validator over the same
      JWKS; fabric-audience tokens are rejected on the data plane and vice
      versa). Workspace RBAC applies: Viewer reads, Contributor writes.
- [x] Managed-folder enforcement (`onelake-api-parity.md`): HEAD-only at
      account/workspace level; item root + first level protected from
      create/rename/delete; `setAccessControl`-class params rejected; banned
      headers ignored + echoed via `x-ms-rejected-headers`; canned
      `$superuser` / `---------` permission response headers.
- [x] Name- and GUID-addressing resolve to the same workspace/item.
- [x] e2e: full write flow (create → append ×2 → flush) via GUID addressing,
      read back via name addressing; listings; RBAC walls; managed-folder
      rejections — against real entra-minted Storage tokens.
- [ ] Shortcuts (thin) + trusted-workspace-access smoke path (later).
- [ ] e2e: azcopy / ADLS SDK against the emulator (later; wire subset ready).

## Cross-cutting (throughout)

- [ ] Svelte portal: workspaces / items / role assignments / operations / git
      status / provisioning views — `go:embed` + committed `dist` + CI drift guard.
- [x] Starlight docs site on GitHub Pages (this `/docs` = source of truth,
      synced by `website/scripts/sync-docs.mjs`; pinned Astro Starlight;
      deploys via `docs-site.yml`) — live at
      <https://calvinchengx.github.io/fabric-emulator/>.
- [ ] GoReleaser: binaries + distroless Docker (GHCR) + Homebrew cask + winget.
- [ ] Playwright headless mount smoke (catch builds-but-doesn't-mount).
- [ ] Coverage parity with entra-emulator (target ≥ 70% per package).

## Sequencing note

Build the **LRO engine before anything that mutates** — every workspace/item/git
call returns through it, so getting `202` → poll → terminal right once makes all
later endpoints trivial. P2's entra-side dependency (#16) has already shipped,
so phase order is a pure prioritization choice, not a blocking one.
