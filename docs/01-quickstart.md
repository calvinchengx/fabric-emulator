# 01 — Quickstart

Five minutes from nothing to a workspace, an item, and a file in OneLake — no
tenant, no capacity. Every value below is a **seeded dev default** from
entra-emulator; nothing needs registering.

## 1. Start the pair

```bash
git clone https://github.com/calvinchengx/fabric-emulator
cd fabric-emulator
docker compose up
```

This brings up **entra-emulator** on `https://localhost:8443` (issues tokens)
and **fabric-emulator** on `https://localhost:9443` (validates them against
entra's JWKS — exactly as real Fabric validates against Entra). Both serve
self-signed TLS, hence `-k` below.

Without Docker: run entra-emulator locally (see its
[quickstart](https://calvinchengx.github.io/entra-emulator/01-quickstart/)),
then

```bash
go run ./cmd/fabric-emulator \
  -entra-issuer "https://localhost:8443/11111111-1111-1111-1111-111111111111/v2.0" \
  -entra-tls-insecure
```

## 2. Mint a Fabric-audience token

entra-emulator seeds a confidential **daemon** app. Client credentials against
the Fabric resource:

```bash
TOKEN=$(curl -sk https://localhost:8443/11111111-1111-1111-1111-111111111111/oauth2/v2.0/token \
  -d grant_type=client_credentials \
  -d client_id=cccccccc-0000-0000-0000-000000000002 \
  -d client_secret=daemon-app-secret \
  -d scope=https://api.fabric.microsoft.com/.default | jq -r .access_token)
```

(The legacy `https://analysis.windows.net/powerbi/api/.default` scope works
too — most fabric-docs samples use it, and both audiences are accepted.)

## 3. Create a workspace — and meet the LRO

Nearly every Fabric mutation is async: `202 Accepted` + poll. The emulator
implements this faithfully:

```bash
curl -sk -D- https://localhost:9443/v1/workspaces \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"displayName": "quickstart"}'
```

A `201` returns the workspace directly (create is one of the sync paths); note
the `id` — and that `capacityId` is already set: the emulator seeds a default
capacity and auto-assigns it, so tools like `fabric-cicd` that refuse
capacity-less workspaces work out of the box.

Async calls (item create with a definition, git sync, …) return `202` with
`x-ms-operation-id` and `Location` headers; poll until the status leaves
`Running`:

```bash
curl -sk https://localhost:9443/v1/operations/<operation-id> \
  -H "Authorization: Bearer $TOKEN"
```

By default operations complete on the next poll. Pin them `Running` with
`-lro-delay` or the [clock control](10-testing.md) to test polling loops.

## 4. Create an item

```bash
curl -sk https://localhost:9443/v1/workspaces/<workspace-id>/items \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"displayName": "lake", "type": "Lakehouse"}'
```

Typed collections (`/lakehouses`, `/notebooks`, …) serve the same items — see
the [control-plane API](07-control-plane-api.md).

## 5. Write a file into OneLake

The data plane wants a **Storage**-audience token and is Host-routed at
`onelake.dfs.fabric.microsoft.com` (the self-signed cert covers that name —
see [TLS & hosts](05-tls-and-hosts.md)):

```bash
STOKEN=$(curl -sk https://localhost:8443/11111111-1111-1111-1111-111111111111/oauth2/v2.0/token \
  -d grant_type=client_credentials \
  -d client_id=cccccccc-0000-0000-0000-000000000002 \
  -d client_secret=daemon-app-secret \
  -d scope=https://storage.azure.com/.default | jq -r .access_token)

OL="https://onelake.dfs.fabric.microsoft.com:9443"
R="--resolve onelake.dfs.fabric.microsoft.com:9443:127.0.0.1"

# create, append, flush — the ADLS Gen2 protocol
curl -sk $R -X PUT "$OL/<workspace-id>/<item-id>/Files/hello.txt?resource=file" -H "Authorization: Bearer $STOKEN"
printf 'hello onelake' | curl -sk $R -X PATCH "$OL/<workspace-id>/<item-id>/Files/hello.txt?action=append&position=0" -H "Authorization: Bearer $STOKEN" --data-binary @-
curl -sk $R -X PATCH "$OL/<workspace-id>/<item-id>/Files/hello.txt?action=flush&position=13" -H "Authorization: Bearer $STOKEN"
curl -sk $R "$OL/<workspace-id>/<item-id>/Files/hello.txt" -H "Authorization: Bearer $STOKEN"
```

Fabric-audience tokens are rejected on the data plane and vice versa, matching
real OneLake. Managed-folder rules apply — try to `DELETE` `/Files` itself and
watch it refuse ([OneLake](08-onelake.md)).

## Where next

- Point the **real `fabric-cicd` tool** at the emulator:
  [testing with fabric-cicd](11-testing-with-fabric-cicd.md).
- Freeze time and inject faults: [testing](10-testing.md).
- Every endpoint: [control-plane API](07-control-plane-api.md) and
  [OneLake](08-onelake.md).
