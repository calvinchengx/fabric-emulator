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

## Left as `go:`

Internal engines, races, and protocols Microsoft's clients never speak:
pipeline activity interpreters, ForEach/retry/control-flow, `-tsql-strict`,
`FABRIC_FORCE_LRO`, concurrent Delta overwrite, notebookutils-over-REST
(real Fabric 404s that path), `runMultiple`, the reference-run lakehouse
rule, git's in-emulator remote, event-trigger firing, materialized-lake-view
refresh, CopyJob execution, HDInsight/Databricks/ADX/Functions/Web activities.
