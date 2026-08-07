# BMC Helix → Lakehouse, through the REST connector

The worked example for [docs/40](../../docs/40-rest-connector-plan.md), and the
end-to-end proof that the REST connector does what it was built for.

## Why Helix

**BMC Helix ITSM has no Fabric connector.** Not in Fabric, not in ADF, not in
Power Query. Its direct competitor ServiceNow has a first-party one; Helix does
not, and Microsoft's own answer for it is a custom or generic connector over its
REST API. That makes it the sharpest possible case for building Fabric's generic
**`RestSource`** rather than inventing a `BMCHelixSource` — a fictional item type
would run here and fail to parse in the product, which is the one direction this
emulator must never diverge.

## What runs

```
Login (Web activity)  ──▶  POST /api/jwt/login          raw JWT in the body
   │
   ▼
Ingest (Copy)         ──▶  RestSource                   AR-JWT header from the
   │                       limit/offset pagination      login output, ARS records
   │                       translator.mappings          unnested from `values`
   ▼
Lakehouse  Tables/bronze_incidents  (Delta)
```

Every activity type here is real in Fabric, and every one executes for real in
the emulator. Nothing is stubbed.

## The three things the stand-in server gets right

`helix.py` is small, and deliberately faithful where it counts. Each of these
would let a convenient fiction pass while the real Helix failed:

| | Real AR System | Why it matters |
|---|---|---|
| **Login response** | the token is the **entire body**, not JSON | the pipeline reads `@activity('Login').output.body`; a JSON envelope would work against a mock and fail against Helix |
| **Auth scheme** | `Authorization: AR-JWT <token>` — **`Bearer` is rejected** | AR-JWT is not one of Fabric's built-in authentication types, so the token must be threaded in as an expression through `additionalHeaders`. The server rejects `Bearer` exactly as the real one does |
| **Record shape** | `{"entries":[{"values":{…},"_links":{…}}]}` | every field is one level down, so auto-flatten finds **no scalar columns at all**. `translator.mappings` is load-bearing, not decorative |

That third row is why mappings exist at all: the example could not have been
written honestly without them.

## What the assertions catch

- **`rowsCopied == 5`** with a page size of 2. A connector that read only the
  first page would report 2 and look perfectly healthy — the count is the
  assertion that only completed pagination can satisfy.
- **`_delta_log` under `Tables/bronze_incidents`**, read back through OneLake:
  the rows really committed as Delta, not just parsed.
- **`lineage.sourceKind == "connection"`** and a `sourcePath` naming the `arsys`
  entry point, so the portal draws Helix as a node outside Fabric rather than
  hunting for an item that does not exist.

## Running it

```bash
uv run --frozen --no-sync python e2e/rest-helix/run.py
```

No credentials and no network beyond the compose project: entra-emulator,
fabric-emulator, the stand-in Helix, and the driver.
