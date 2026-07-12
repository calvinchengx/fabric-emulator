# 03 — Roadmap

Scope chosen: **full** — control plane through the OneLake data plane, matching
entra-emulator's surfaces (portal + Starlight docs + distribution), composed via
docker-compose. Each phase is independently useful and CI-verified (real-SDK
e2e, like entra-emulator's SDK matrix).

## P0 — the spine (token acceptance + workspaces + items + RBAC + LRO)

The minimum that lets someone test **SP → Fabric client-credentials** automation.

- [ ] Token acceptance: validate Bearer against entra-emulator JWKS/issuer;
      audience set; `oid`/`appid` extraction. (`--entra-issuer`, `--entra-jwks-url`)
- [ ] Store + migrations (`workspace`, `item`, `role_assignment`, `operation`).
- [ ] **LRO engine** on the controllable clock (`202`/`Location`/`Retry-After`,
      `GET /operations/{id}`). This is the fidelity centerpiece — build it first.
- [ ] Workspaces CRUD + `assignToCapacity`.
- [ ] Generic items CRUD.
- [ ] RBAC: role assignments CRUD + enforcement (Admin/Member/Contributor/Viewer).
- [ ] Fault injection + clock control (mirror entra-emulator).
- [ ] Health/ready; Docker image; docker-compose with entra-emulator.
- [ ] e2e: Fabric SDK (`microsoft.fabric` / raw REST) list-create-delete workspace.

## P1 — CI/CD (the primary draw)

Makes `fabric-cicd`, git integration, and deployment pipelines run offline.

- [ ] Item **definitions**: `getDefinition` / `updateDefinition` (parts round-trip).
- [ ] Typed item aliases (notebook, lakehouse, warehouse, dataPipeline, semanticModel…).
- [ ] **Git integration**: connect / initializeConnection / status / commitToGit /
      updateFromGit / disconnect, backed by a local per-branch definition store.
- [ ] Jobs: instances trigger + state transitions + cancel.
- [ ] e2e: drive `fabric-cicd` against the emulator; assert a workspace round-trips
      through commit → update.

## P2 — the identity handshake (deepest entra integration)

The "works seamlessly with entra-emulator" payoff. **Depends on entra-emulator
roadmap #16** (workspace-identity object + token minting).

- [ ] Workspace-identity lifecycle: create workspace → drive entra-emulator's
      workspace-identity object (name-follows-workspace, cascade delete,
      `Active/Inactive/Deleting/Unusable/Failed/DeleteFailed` state enum).
- [ ] Outbound token minting: when an item needs a token, ask entra-emulator's
      forge to mint one for that identity (customer never sees a credential).
- [ ] Audit event parity: `Retrieved Fabric Identity Token for Workspace`.
- [ ] e2e: workspace create → identity Active → mint token → call back into
      fabric-emulator with it.

## P3 — OneLake data plane

- [ ] `onelake.` host mux: ADLS-Gen2/Blob subset (create/append/flush/read/list/delete).
- [ ] `Storage`-audience token acceptance.
- [ ] Name- and GUID-addressing resolve to the same item.
- [ ] Shortcuts (thin) + trusted-workspace-access smoke path.
- [ ] e2e: azcopy / ADLS SDK writes a file into a lakehouse, reads it back.

## Cross-cutting (throughout)

- [ ] Svelte portal: workspaces / items / role assignments / operations / git
      status / provisioning views — `go:embed` + committed `dist` + CI drift guard.
- [ ] Starlight docs site on GitHub Pages (this `/docs` = source of truth).
- [ ] GoReleaser: binaries + distroless Docker (GHCR) + Homebrew cask + winget.
- [ ] Playwright headless mount smoke (catch builds-but-doesn't-mount).
- [ ] Coverage parity with entra-emulator (target ≥ 70% per package).

## Sequencing note

Build the **LRO engine before anything that mutates** — every workspace/item/git
call returns through it, so getting `202` → poll → terminal right once makes all
later endpoints trivial. P2 should not start until entra-emulator #16 ships.
