# Azure CLI stranger (`az rest`)

Microsoft's own `az` CLI completing `az login --service-principal` against
entra-emulator, then `az rest` against **this** fabric checkout. The
fabric-cli job already drives `fab`; this is the packaged CLI a developer
runs, speaking Fabric REST and the Power BI admin activity log — surfaces
fab's typed verbs do not cover.

```sh
python3 e2e/az-rest/run.py
```

Needs Docker. The client image is `mcr.microsoft.com/azure-cli` at a pinned
tag; nothing in it is patched. entra is `login.microsoftonline.com` and
fabric is `api.fabric.microsoft.com` (MSAL drops a non-443 port). `az login`
still lists subscriptions; a one-route HTTPS stub answers with an empty
list — the same pattern entra's az-cli job uses. It is not a witness of ARM.

## What it witnesses

Existing 🟢 rows only. The ledger does not grow.

| claim | how |
|---|---|
| Capacities (list, assign / unassign) | `GET /v1/capacities`; unassign then assign; workspace `capacityId` follows |
| Folders | create, nest, list |
| Role assignments / workspace RBAC | grant Viewer, list, delete |
| Item job scheduler | Cron schedule create / get / list / delete (not the emulator clock) |
| List item job instances | on-demand Pipeline run, then `GET …/jobs/instances` |
| `queryactivityruns` | POST against that instance; documented `value` envelope |
| Tenant settings | GET documented sample names; POST update |
| Tenant-wide workspace / item admin | documented envelopes (`workspaces`, `itemEntities`) |
| Capacity tenant-setting overrides | GitIntegration override round-trip |
| Governance domains | create / list / delete |
| Sensitivity labels | `bulkSetLabels` / `bulkRemoveLabels` |
| Activity log / audit | `GET /v1.0/myorg/admin/activityevents` after real operations |
| Workspace managed identity handshake | `provisionIdentity` → identity on the workspace → `deprovisionIdentity`; entra is the sibling |
| Power BI Report items | create with a PBIR definition; `getDefinition` returns the same bytes |
| Deleted display names held | create / delete / recreate → `409 ItemDisplayNameNotAvailableYet` with `isRetriable: true` |
| Git integration (connect / status / commit / update / disconnect) | connect with a `ConfiguredConnection`; `initializeConnection` → `CommitToGit`; status shows the Added item; commit; status clean with a remote hash; a **second** workspace connects, gets `UpdateFromGit`, pulls the item, and its definition still has `.platform`; disconnect |
| `CopyJob` — it really copies | seed `Tables/orders/part-0.parquet` over OneLake, create the CopyJob with its `copyjob-content.json` definition, run `jobType=Execute`, poll the instance to `Completed`, then read `Tables/bronze_orders/part-0.parquet` back and compare the bytes |

## Left as `go:`

Internal engines, races, and protocols Microsoft's clients never speak:
pipeline activity interpreters, ForEach/retry/control-flow, `-tsql-strict`,
concurrent Delta overwrite, notebookutils-over-REST
(real Fabric 404s that path), `runMultiple`, the reference-run lakehouse
rule, event-trigger firing, materialized-lake-view refresh,
HDInsight/Databricks/ADX/Functions/Web activities.

**Git integration and CopyJob used to be on that list and are not any more,
which is worth recording because the reason they were on it was half right.**

Git was listed as "git's in-emulator remote". The remote *is* emulated — but
the claim is `connect / status / commit / update / disconnect`, and every one
of those is public Fabric REST that `az` speaks. The emulated remote is what
the emulator IS; it no more disqualifies the witness than the emulated capacity
disqualifies the capacities row. What the witness proves is the documented
contract and its state machine: `initializeConnection` demanding `CommitToGit`
against a virgin remote and `UpdateFromGit` for a second workspace on a
populated one, and definitions surviving the round trip.

CopyJob was listed as "CopyJob execution", and the engine really is out of
reach. But the claim is that a Copy Job **copies**, and driving the run over
REST to read back `Completed` would have witnessed the job contract while
proving nothing about the data — the exact shape of false green this repo
keeps finding. There is no public Fabric REST route listing a lakehouse's
tables (the VS Code one is MWC-authenticated and internal), so the witness
seeds the source and reads the destination over **OneLake**, which is the same
CLI and the same login with `--resource https://storage.azure.com` instead of
the Fabric audience. Bytes in, bytes out, or it fails.

`FABRIC_FORCE_LRO` has since left the list too, and for a third variant of the
same reason. It was listed because the TOGGLE is emulator-only — no tenant has
a switch that forces the async outcome — which is true and beside the point:
what the toggle produces is the documented 202 + `Location` + `Retry-After`
contract, and a client with its own poll loop either follows it or does not.
`az rest` is the wrong client for it (it returns the 202 and stops, so the
polling would be this repo's), so the witness lives where a real poll loop
already exists: `e2e/fabric-cicd` run with the toggle on, as the
`fabric-cicd-lro` job.

The general lesson for anything else on the list above: separate "the engine is
internal" from "the contract is internal". The first is a real ceiling. The
second is often just an untried client. And when the second holds, the witness
does not have to be *this* suite — it belongs wherever a client already speaks
the contract.
