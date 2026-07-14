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

**Working** — the contract spine (P0–P3) *and* the real-compute track (R0–R5)
are shipped and CI-verified on Linux, macOS, and Windows.

- **Contract plane:** workspaces, items, RBAC, deterministic LROs; the CI/CD
  surface (definitions, typed aliases, connections + credential model, git
  integration, jobs — the real `fabric-cicd` tool publishes unmodified); the
  workspace-identity handshake with entra-emulator; and the OneLake ADLS-Gen2 +
  Blob data plane (managed folders, Delta put-if-absent commits, shortcuts).
- **Real compute (opt-in sidecars):** real **Spark** over a native Livy agent
  (interactive + high-concurrency sessions, notebook cell execution, Delta via
  ABFS); real **T-SQL over TDS** with Entra **FedAuth** terminated and the
  session byte-spliced to a **SQL Server** sidecar — driven by both `go-mssqldb`
  and Microsoft **ODBC Driver 18** (Microsoft's real `dbt-fabric` adapter passes
  end-to-end); **DuckDB** SQL over lakehouse Delta; and a pure-Go **pipeline**
  interpreter with real leaf activities. Real clients (delta-rs, the Azure Blob
  SDK, azcopy, PySpark, dbt) drive it in CI as borrowed oracles.

The default binary runs none of the engines (clock-derived, milliseconds);
each is an opt-in flag/sidecar. Coverage floor is 90% (currently ~95%).

Docs: <https://calvinchengx.github.io/fabric-emulator/> — start with
[architecture](docs/03-architecture.md), the
[control-plane API](docs/07-control-plane-api.md), [OneLake](docs/08-onelake.md),
the [roadmap](docs/13-roadmap.md), [real compute](docs/14-real-compute.md), the
[warehouse over TDS](docs/16-warehouse-tds.md), and the
[parity table](docs/17-parity.md).

## Relationship to entra-emulator

The two projects are decoupled — fabric-emulator depends on entra-emulator
**only over HTTP** (JWKS + issuer, plus a token-mint call for workspace
identities). A [`docker-compose.yml`](docker-compose.yml) brings up both with
fabric pre-wired to entra's issuer. It could equally point at a real Entra
tenant.

`docker compose up` attaches **real engines by default** — a Spark agent and a
SQL Server sidecar, via the auto-loaded
[`docker-compose.override.yml`](docker-compose.override.yml) — so Livy
sessions, notebook cells, and the T-SQL/TDS warehouse surface run for real out
of the box. `docker compose -f docker-compose.yml up` opts out to the lite,
contract-only pair.

## License

Apache-2.0. Clean-room: built only from public documentation
([`MicrosoftDocs/fabric-docs`](https://github.com/MicrosoftDocs/fabric-docs)) and
public REST references — no Microsoft source.
