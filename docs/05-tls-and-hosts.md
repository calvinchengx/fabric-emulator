# 05 — TLS & hosts

fabric-emulator serves **one listener** with **Host-header routing**, exactly
like real Fabric's split between the control plane and OneLake:

| `Host` starts with | Serves |
|---|---|
| `onelake.` | the [OneLake data plane](08-onelake.md) (ADLS-Gen2 DFS) |
| anything else | the [`/v1` control plane](07-control-plane-api.md), `/health`, and the [`/_emulator` testing surface](10-testing.md) |

So plain `https://localhost:9443/v1/...` always reaches the control plane, and
the data plane needs the request to *carry the OneLake hostname*.

## The certificate

On first start the emulator generates (and, with `FABRIC_DATA_DIR`, persists)
a self-signed certificate covering:

```
localhost
fabric-emulator
api.fabric.microsoft.com
onelake.dfs.fabric.microsoft.com
```

Covering the **real Fabric hostnames** is deliberate: it lets unmodified
tools talk to the emulator under the names they insist on, with only DNS-level
redirection — no code changes, no cert warnings beyond the self-signed root.

## Three ways to send the right Host

1. **curl `--resolve`** (no system changes):

   ```bash
   curl -k --resolve onelake.dfs.fabric.microsoft.com:9443:127.0.0.1 \
     https://onelake.dfs.fabric.microsoft.com:9443/<ws>/<item>/Files/f.txt ...
   ```

2. **`/etc/hosts`** — `127.0.0.1 api.fabric.microsoft.com
   onelake.dfs.fabric.microsoft.com` makes every tool on the machine hit the
   emulator under the real names.

3. **In-process DNS pin** — what the [fabric-cicd e2e](11-testing-with-fabric-cicd.md)
   does: monkey-patch `socket.getaddrinfo` (Python) to map the Fabric hostnames
   to `127.0.0.1` before any socket opens. Scoped to one process, needs no
   privileges, works in CI.

The port stays in the URL (`https://api.fabric.microsoft.com:9443`) — tools
that validate the *hostname* accept any port, which is exactly what makes the
DNS-pin approach work for `fabric-cicd`.

## TLS toward entra-emulator

fabric-emulator is also a TLS *client* — it fetches entra's JWKS. entra serves
a self-signed cert too, so on the compose network `FABRIC_ENTRA_TLS_INSECURE=true`
skips verification for that one connection. Against real Entra, leave it off.

Issuer/JWKS derivation and the advertised-origin alignment that trips people
up are covered in [configuration](04-configuration.md).

## Plain HTTP

`FABRIC_DISABLE_TLS=true` serves HTTP on the same single-listener, Host-routed
model. The `healthcheck` subcommand tries HTTPS first and falls back to HTTP,
so it works in either mode.
