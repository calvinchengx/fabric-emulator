# fabric-emulator

A clean-room, local emulator of the **Microsoft Fabric control plane** (and a
thin OneLake data plane), built to compose with
[entra-emulator](https://github.com/calvinchengx/entra-emulator).

Real Fabric layers two independent systems: **Entra ID** issues the bearer
tokens, and the **Fabric control plane** (`https://api.fabric.microsoft.com/v1/…`)
serves workspaces, item CRUD, RBAC, git integration and long-running operations.
`entra-emulator` already emulates the first. `fabric-emulator` emulates the
second — and validates every incoming token against entra-emulator's JWKS,
**exactly as real Fabric validates against Entra**.

```
 client / SDK ──Bearer(aud=api.fabric.microsoft.com)──▶ fabric-emulator
                                                          │  validates token
                                                          ▼
                                                     entra-emulator  (JWKS, issuer)
```

## Why

- **Test Fabric CI/CD with no capacity.** `fabric-cicd`, git integration, and
  deployment pipelines drive item `getDefinition`/`updateDefinition` and
  `git/commitToGit`/`updateFromGit`. Point them at `localhost` instead of a paid
  tenant.
- **Test service-principal automation.** SP → Fabric client-credentials against a
  real (emulated) issuer, deterministic and offline.
- **Deterministic long-running operations.** Every Fabric mutation is async
  (`202` → poll `/v1/operations/{id}`). The emulator's clock control makes an LRO
  complete instantly or pins it in `Running` — impossible against real Fabric.

## Status

**Working** — phases P0–P3 are shipped and CI-verified on Linux, macOS, and
Windows: the control-plane spine (workspaces, items, RBAC, deterministic LROs),
the CI/CD surface (definitions, typed aliases, connections, git integration,
jobs — the real `fabric-cicd` tool publishes into the emulator unmodified),
the workspace-identity handshake with entra-emulator, and the OneLake
ADLS-Gen2 data plane with managed-folder enforcement. Every package covers
itself (≥77%, 91%+ total).

Docs: <https://calvinchengx.github.io/fabric-emulator/> — start with
[architecture](docs/03-architecture.md), the
[control-plane API](docs/07-control-plane-api.md), [OneLake](docs/08-onelake.md),
and the [roadmap](docs/13-roadmap.md).

## Relationship to entra-emulator

The two projects are decoupled — fabric-emulator depends on entra-emulator
**only over HTTP** (JWKS + issuer, plus a token-mint call for workspace
identities). A [`docker-compose.yml`](docker-compose.yml) brings up both with
fabric pre-wired to entra's issuer. It could equally point at a real Entra
tenant.

## License

Apache-2.0. Clean-room: built only from public documentation
([`MicrosoftDocs/fabric-docs`](https://github.com/MicrosoftDocs/fabric-docs)) and
public REST references — no Microsoft source.
