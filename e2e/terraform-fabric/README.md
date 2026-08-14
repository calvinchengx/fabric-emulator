# Terraform stranger (`microsoft/fabric`)

Microsoft's own [`terraform-provider-fabric`](https://registry.terraform.io/providers/microsoft/fabric)
completing `terraform apply` against **this** fabric checkout. The az-rest job
already drives `az rest`; this is the Terraform provider a developer runs,
speaking Fabric REST for workspaces, folders, a Lakehouse, capacities, and
workspace RBAC.

```sh
python3 e2e/terraform-fabric/run.py
```

Needs Docker and network (`terraform init` pulls the pinned provider from the
registry). The client image is `hashicorp/terraform` at a pinned tag; nothing
in Terraform or the provider is patched. entra is `login.microsoftonline.com`
and fabric is `api.fabric.microsoft.com` (the Go Azure SDK drops a non-443
port). Instance discovery still talks to the real `login.microsoft.com`; only
the token request hits entra.

This is Terraform talking Fabric REST, not ARM. The dedicated provider is not
`azurerm`.

## What it witnesses

Existing 🟢 rows only. The ledger does not grow.

| claim | how |
|---|---|
| Capacities (list, assign / unassign) | `data.fabric_capacity` lists the seeded F64; workspace `capacity_id` assigns it. Unassign stays `az-rest` / `go:` |
| Workspaces CRUD | `fabric_workspace` create / read / destroy |
| Folders | root folder plus a nested `parent_folder_id` |
| Role assignments / workspace RBAC | grant Viewer |
| Items CRUD + typed collections | `fabric_lakehouse` create in that folder (typed `/lakehouses` collection) |

`fabric_folder` is preview in provider 1.12.1, so the provider block sets
`preview = true` (or `FABRIC_PREVIEW`). That is the documented switch, not a
patch.

The seeded capacity's `region` is `West Europe` — a value in the provider's
region enum — because `local` is not a Fabric capacity region and the data
source would refuse it.

## Left as `go:` / other `ci:`

Internal engines, races, and protocols this provider does not speak:
pipeline activity interpreters, Spark, OneLake bytes, git, deployment
pipelines, shortcuts, workspace identity provision (az-rest already
witnesses that handshake), ARM SKU/billing (`ci:arm-capacities`).
