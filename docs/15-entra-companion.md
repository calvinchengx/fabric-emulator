# 15 — Companion: entra-emulator

fabric-emulator is one half of a pair. The identity half —
[entra-emulator](https://github.com/calvinchengx/entra-emulator)
([docs](https://calvinchengx.github.io/entra-emulator/)) — is a local,
MSAL-compatible Entra ID (Azure AD) STS. This page is the map of the seam,
from this side; entra's own
[fabric-companion doc](https://calvinchengx.github.io/entra-emulator/18-fabric-companion/)
is the same map from the other side.

## What fabric-emulator consumes from entra-emulator

All of it **over plain HTTP** — no shared code, no shared process:

| entra surface | Used for |
|---|---|
| `/{tenant}/discovery/v2.0/keys` (JWKS) + the advertised issuer | validating every bearer token, control plane and OneLake alike ([architecture](03-architecture.md)) |
| Fabric-audience token minting (`https://api.fabric.microsoft.com` and the legacy Power BI resource) | what clients authenticate with |
| `Storage`-audience tokens (`https://storage.azure.com`) | the [OneLake data plane](08-onelake.md) |
| `vault`-audience tokens (`https://vault.azure.net`), minted for the workspace identity | resolving [Azure Key Vault references](09-identity-handshake.md) against azure-keyvault-emulator |
| `/admin/api/workspace-identities` (CRUD) + `GET /fabric/workspaceidentities/{id}/token` | the [workspace-identity handshake](09-identity-handshake.md) |

Because the dependency is only "an issuer + a JWKS", `FABRIC_ENTRA_ISSUER`
can point at a **real Entra tenant** instead and everything except the
workspace-identity orchestration works unchanged.

## Running the pair

- **docker-compose** (recommended): the repo's
  [`docker-compose.yml`](../docker-compose.yml) starts both, issuer-aligned
  (entra advertises the compose-internal origin — the subtlety explained in
  [configuration](04-configuration.md)).
- **Locally**: entra on `:8443`, fabric on `:9443` with
  `-entra-issuer https://localhost:8443/{tenant}/v2.0 -entra-tls-insecure`.
- **In-process (Go tests)**: both emulators in one test binary — how this
  repo's own e2e runs ([e2e matrix](12-e2e-matrix.md)).

## The family

The third member is integrated:
[azure-keyvault-emulator](https://github.com/calvinchengx/azure-keyvault-emulator)
backs Fabric's **Azure Key Vault references** for connection credentials
(`workspace identity → entra vault-audience token → vault secret → connection`).
Like fabric, it is a relying party on entra — it validates bearers against the
same JWKS/issuer, with the vault audience. The full trust edge is drawn in the
[identity handshake](09-identity-handshake.md#reaching-a-protected-resource-azure-key-vault);
it also serves `notebookutils.credentials.getSecret`. Brought up together in the
`notebookutils` e2e (all three emulators + a real notebook reading a secret).
