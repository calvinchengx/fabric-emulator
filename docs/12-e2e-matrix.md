# 12 — E2E matrix

What CI proves on every push, and what's queued. The bar, inherited from
entra-emulator's SDK matrix: **real clients, unmodified**, against the
emulator — because driving Microsoft's actual tools catches fidelity gaps
spec-reading cannot (see [what fabric-cicd caught](11-testing-with-fabric-cicd.md#what-driving-the-real-tool-caught)).

## Verified on every push

| Suite | Client | Proves | Where |
|---|---|---|---|
| Token handshake | in-process **entra-emulator** | real client-credentials token (Fabric aud) → JWKS validation → full workspace/RBAC/item/LRO flow over HTTP | Go integration tests (CI `test` job, Linux + macOS + Windows) |
| Git round-trip | Go HTTP | two-workspace commit→update, definitions intact, logical ids preserved | Go integration tests |
| Identity handshake | in-process entra-emulator | provision → entra mints for the identity → token passes fabric RBAC → deprovision revokes → delete cascades | Go integration tests |
| OneLake | Go HTTP + real entra Storage tokens | create/append/flush/read via GUID + name addressing, listings, RBAC walls, managed-folder rejections | Go integration tests |
| **fabric-cicd** | Microsoft's real Python tool (v1.2.x) | `publish_all_items` publishes a notebook; parts round-trip byte-for-byte | `e2e/fabric-cicd/run.py` (CI `fabric-cicd` job) |

Plus: coverage floor 90% (cross-package; currently ~93%), `go vet`, and the
[docs site](https://calvinchengx.github.io/fabric-emulator/) build on every
docs push.

## Queued (designed, not yet wired)

| Suite | Client | Proves | Blocked on |
|---|---|---|---|
| Delta write/read (**A1**) | `deltalake` (delta-rs) | a real engine writes a Delta table through our DFS with an entra Storage token | R0 storage completeness ([14-real-compute.md](14-real-compute.md)) |
| Spark via ABFS (**A2**) | real PySpark + delta-spark | `abfss://…@onelake.dfs…` + OAuth against entra; cross-engine read-back with delta-rs | R1 |
| azcopy / ADLS SDK | Microsoft storage tooling | the DFS wire subset satisfies stock ADLS clients | subsumed by A1/A2 |

## Running locally

```bash
go test ./...              # everything in-process, no network
python3 e2e/fabric-cicd/run.py   # the real-tool e2e (needs Python 3 + go)
```

Both are deterministic: virtual clock, in-memory stores, seeded credentials.
