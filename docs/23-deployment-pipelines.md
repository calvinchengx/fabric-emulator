# 23 — Deployment pipelines (P1's third leg)

**Status: D0–D3 shipped — the surface is complete.** The pipeline/stage model
and its read surface are real state (`internal/store/deployment.go`,
`internal/api/deployment.go`), as is workspace assignment with real item
pairing (`internal/store/pairing.go`) and **Deploy Stage Content**
(`internal/store/deploy.go`, over the existing LRO engine) and the
role-assignment CRUD. Nothing here pretends to deploy — those endpoints are absent
rather than stubbed.

[13-roadmap.md](13-roadmap.md)'s P1 header reads: *"Makes `fabric-cicd`, git
integration, and **deployment pipelines** run offline."* Two of those three
shipped. This is the third, and it is the only one recorded solely as a red row
in [parity.md](parity.md) rather than as an unchecked box — so it has been
invisible in the roadmap's own accounting.

Fabric has exactly two CI/CD mechanisms: **git integration** (branch ↔
workspace, shipped at P1) and **deployment pipelines** (stage → stage
promotion). Shipping one and calling P1 done keeps half the promise.

## Grounding

Structure and semantics come from `MicrosoftDocs/fabric-docs` pinned at
**`0d63906ac29d8e8befa42b13f3d1d31c0f92081a`** (2026-07-10):
`docs/cicd/deployment-pipelines/{intro-to-deployment-pipelines,assign-pipeline,
understand-the-deployment-process,pipeline-automation-fabric}.md`.

Exact request/response JSON is **REST-reference-only**
(`/rest/api/fabric/core/deployment-pipelines`) — the same convention already
used for capacities in [07-control-plane-api.md](07-control-plane-api.md#L28).
fabric-docs carries the conceptual model, not the wire schema. Wire shapes
therefore get confirmed against the REST reference at implementation time, and
against real Fabric once the conformance oracle
([21-real-fabric-toggle.md](21-real-fabric-toggle.md)) is armed.

## The contract in one paragraph

A pipeline has **2–10 ordered stages** (default 3: Development / Test /
Production). Each stage may have **at most one workspace** assigned. Deploying
stage *N* → *N+1* copies **item metadata only** into the target workspace:
*paired* items are overwritten in place, *unpaired* items are clean-deployed
(created, then paired). **Data is never copied.** Items present in the target
but absent from the source are left alone — deployment is not a mirror, unlike
`updateFromGit`.

That last clause matters: our git implementation deletes stale items on
`updateFromGit`. Deployment must **not**. Reusing the git mirror logic here
would be an easy and wrong shortcut.

## What deploy copies — and doesn't

From `understand-the-deployment-process.md`:

| Copied | Not copied |
|---|---|
| Item definition / metadata | **Data** (OneLake bytes — metadata only) |
| Data sources, parameters | Item **ID** (target keeps its own) |
| Model metadata, item relationships | **Permissions** (workspace or item) |
| Sensitivity labels (conditionally) | Workspace settings |
| Folder hierarchy | URL, personal bookmarks |

This maps almost entirely onto machinery already built. "Copy the definition,
keep the target's ID, don't touch role assignments, don't touch OneLake" is
`getDefinition` → `updateDefinition` (P1, shipped) plus a pairing table. The
genuinely new logic is pairing and the stage model — not deployment itself.

Concretely: deploying a Lakehouse copies the *item*, not its Delta tables. A
target-stage lakehouse comes up empty. That is correct behaviour, and it is the
kind of thing an emulator is tempted to "helpfully" get wrong.

## Pairing is persistent state, not a name match

This is the fidelity core, and the easiest thing to implement incorrectly.

Pairing happens two ways (`assign-pipeline.md`):

**On workspace assign** — match candidates on `(item name, item type)`, with
**folder location as a tie-breaker** when a stage holds duplicates. One match
each side → paired. Ambiguous (two same-name/type/folder items) → **pairing
fails, and deployment then fails**.

**On deploy** — an unpaired source item is copied into the target and the copy
is paired with it (a "clean deploy").

Then the rule that forces the design:

> Once items are paired, renaming them *doesn't* unpair the items. Thus, there
> can be paired items with different names.

So a pair is a **stored edge between two item IDs**, established once and
surviving renames on either side. An implementation that re-derives pairs by
name at deploy time would pass every test written against freshly-created
items and silently break the moment anyone renames one — the failure mode being
a *duplicate item created in production* rather than an overwrite. Pairs get
their own table, written at assign/deploy time and never recomputed.

## Two open fidelity questions

Both are load-bearing, neither is resolvable from the conceptual docs. Recorded
here rather than guessed at, and both are natural first questions for the
conformance oracle.

**Q1 — do duplicate display names inside one workspace exist?** `28e4a4c` made
`(workspace_id, display_name, type)` a **database UNIQUE constraint**, grounded
in Fabric's real `ItemDisplayNameAlreadyInUse` error on item create. But
`assign-pipeline.md` scenario 3 describes a clean deploy leaving *"two files in
stage B with the same name — one paired and one unpaired"*, and the whole reason
pairing needs a folder tie-breaker is that duplicates are assumed possible. The
examples are all `PBI Report` — a Power BI item type, which historically
permitted duplicate names where Fabric-native items do not. So the two may
simply be describing different item families.

*Proposed default:* keep the constraint, and have a deploy that would create a
duplicate **fail loudly** rather than silently skip or silently rename. An
honest failure is recoverable and reportable; a silent divergence in a
promotion path is not. Consistent with the R-track rule — *never fake results,
either do it for real or fail honestly.*

**Q2 — does deploy overwrite the target's display name?** If it does, renames
are undone on every deploy and "paired items can have different names" holds
only until the next promotion. If it doesn't, the target keeps its name
permanently. The docs assert the latter's *possibility* without saying which
side wins on deploy. *Proposed default:* do **not** overwrite the target's
display name, since that is the only reading under which the documented
statement stays true over time.

## API surface

Eighteen operations (`pipeline-automation-fabric.md`), grouped by phase:

| Phase | Operations |
|---|---|
| **D0** ✅ | Create / Get / Update / Delete / List Deployment Pipelines; List + Get + Update Stage; List Stage Items |
| **D1** ✅ | Assign Workspace To Stage; Unassign Workspace From Stage; pairing on assign |
| **D2** ✅ | **Deploy Stage Content** (202 LRO), deploy-all and selective; Get / List Deployment Pipeline Operations |
| **D3** ✅ | Add / Delete / List Deployment Pipeline Role Assignments |

D2 also confirmed the design's claim that deployment is mostly reuse: the
copy is `GetDefinition` → `CreateItem`/`SetDefinition`, both P1 machinery.
The new logic is entirely in *what not to do* — the three rules at the top of
`deploy.go`. Deploy runs in both directions between adjacent stages (a
backward deploy reads the same stored pairs in the other orientation).

`Deploy Stage Content` returns through the **existing LRO engine** — `202` +
`x-ms-operation-id` + `Location` + `Retry-After`, terminal state via
`GET /operations/{id}`, with extended deployment detail on `/result` (real
Fabric retains it 24h; we can retain it for the process lifetime). No new
async machinery.

The stage's `workspace_id` carries an `ON DELETE SET NULL` foreign key, so
deleting a workspace unassigns the stage rather than leaving it pointing at
nothing; `workspaceName` is resolved live at read time, so a workspace rename
shows through without a write to the stage.

### What D1 found: the ambiguity case is structurally unreachable

Implementing pairing turned Q1 from a question into a measurement. The
documented ambiguous case — duplicates disambiguated by folder — cannot occur
here, for **two independent reasons**:

1. `(workspace_id, display_name, type)` is a UNIQUE index (`28e4a4c`), so two
   items with the same name *and* type cannot coexist in one workspace.
2. Items carry **no folder membership** in this model, so the documented
   tie-breaker has no data to tie-break with.

`PairItems` is therefore written as a **pure function** over two item sets,
separate from the database, and the ambiguous branch is unit-tested against
hand-built inputs the store cannot currently produce. Writing it as "assume
uniqueness" would have been shorter and would have silently mis-paired the
day either premise changed — which is precisely the failure this design
exists to prevent. Ambiguity fails the assignment (409
`DeploymentPipelineStagePairingFailed`) rather than assigning unpaired, since
an unpaired promotion path duplicates silently on the next deploy.

Assignment also requires the caller to hold **Admin on the workspace** being
attached, not merely on the pipeline — otherwise pipeline access alone would
let anyone pull someone else's workspace into a promotion path.

### Store

```
deployment_pipelines           id, display_name, description
deployment_pipeline_stages     id, pipeline_id, order, display_name,
                               description, is_public, workspace_id NULL
deployment_pipeline_pairs      pipeline_id, source_stage_id, source_item_id,
                               target_stage_id, target_item_id
deployment_pipeline_operations id, pipeline_id, source/target stage, note,
                               status, timestamps, per-item detail
deployment_pipeline_role_assignments  pipeline_id, principal_id, role
```

Stage count is constrained to 2–10 at create. `order` is dense and contiguous;
deployment is only ever defined between **adjacent** stages.

### Access control

Pipelines carry their own RBAC, separate from the workspaces their stages
point at. The creator becomes Admin; List returns only pipelines the caller
holds a role on; a non-member gets **404, not 403**, matching workspaces here.
**Admin is the only role a deployment pipeline defines** — there is no
Member/Contributor/Viewer — so any other value is rejected rather than stored
as something meaningless.

Reads need membership; changing *who can reach the pipeline* needs Admin.
Without that split, any member could revoke the owner.

Two deliberate non-inventions:

* **Assignment requires Admin on the workspace** being attached, not just on
  the pipeline — otherwise pipeline access would be a route to pulling
  someone else's workspace into a promotion path.
* **No last-Admin guard.** Revoking the final Admin orphans a pipeline. The
  workspace implementation here has the same property, so matching it is
  consistent rather than inventing a rule the REST reference has not been
  checked against. Worth confirming with the conformance oracle.

## Out of scope, with cause

* **Deployment rules** (`create-rules.md`) — data-source and parameter
  rebinding. There is **no Fabric REST surface** for them; they exist only in
  the UI and the legacy Power BI API. Nothing to conform to, so nothing to
  build.
* **Dataflows** — `pipeline-automation-fabric.md` states outright that
  dataflows are not supported by the Fabric deployment API.
* **`allowPurgeData` / `allowTakeOver` /
  `allowSkipTilesWithMissingPrerequisites`** — documented as *absent* from the
  Fabric API; Power BI API only.
* **Power BI-only item types** (dashboards, paginated reports, org apps) — not
  modelled by this emulator generally.

Each of these is a place where the honest move is a documented absence rather
than a plausible-looking implementation.

## Testing

**Shipped:** store tests, handler tests, a real-mux test with an entra-minted
bearer token, and a **real-client e2e** — `e2e/fabric-cli` drives the whole
promotion flow with Microsoft's own `fab` CLI (CI job
`fabric-cli`). `fab` 1.6.1 ships no deployment-pipeline verbs (nothing in the
package mentions them), so those calls go through `fab api` — still fab's
MSAL auth and HTTP stack, just untyped.

Fetching Microsoft's `DeploymentPipelines-DeployAll.ps1` confirmed the wire
contract independently of the REST reference. It calls exactly
`GET /deploymentPipelines` → `GET …/stages` → `POST …/deploy`
(`{sourceStageId, targetStageId, note}`) → `GET /operations/{id}` (honouring
`Retry-After`) → `GET /operations/{id}/result`, which is what D0–D2 built. The
e2e follows that same call order deliberately.

### Running Microsoft's PowerShell sample

**Done** — `e2e/deployment-pipelines` (CI job `deployment-pipelines-ps`) runs
`Connect-AzAccount -ServicePrincipal` + `Get-AzAccessToken` against
entra-emulator and then DeployAll's REST sequence. This is a **second,
independent client family**: the `fab` e2e exercises MSAL's Python
implementation, this one exercises MSAL's .NET implementation via Az.Accounts.

Four constraints were measured rather than assumed, and each one is a real
limit on what an emulator can get away with:

1. **MSAL refuses a non-HTTPS authority outright** — *"Authority host must be
   a TLS protected (https) endpoint."* The plain-HTTP overlay that unblocked
   OpenMetadata's JVM (§22) is not available here.
2. **MSAL drops a non-443 port from the authority.** An authority of
   `https://host:18443/` produces `Connection refused (host:443)`. So entra
   must answer on `:443` with no port in the URL — the same reason the `fab`
   e2e aliases it as `login.microsoftonline.com`. Same shape as the
   Hadoop-ABFS limitation recorded in [13-roadmap.md](13-roadmap.md) (R1+R2).
3. **No certificate-validation bypass exists.** `Connect-AzAccount`'s
   `-SkipValidation` is about environment metadata, not TLS, so the
   emulator's roots go into the container's trust store for real.
   (`SSL_CERT_FILE` is ignored on macOS — .NET uses the keychain there.)
4. **`-ResourceManagerUrl` is mandatory** (omitting it throws *"Value cannot
   be null. (Parameter 'uriString')"*), yet there is no ARM in this family.
   It must not point at an origin root — see below.

#### A cross-repo finding: an SPA fallback breaks strict API clients

Pointing `-ResourceManagerUrl` at the emulator made Az die with *"Unexpected
character encountered while parsing value: `<`"*. The cause: Az probes
`{ResourceManagerUrl}/metadata/endpoints`, and **entra-emulator's portal SPA
answers any unknown root-level GET with `200 text/html`**. A `404` would have
been survivable; a `200` web page is not, and the error names nothing useful.

**fabric-emulator had the same bug, and worse** — even `/v1/nonsense` returned
`200 text/html`, where real Fabric returns a JSON error. Fixed here: paths
under an API prefix (`/v1/`, `/_emulator/`, `/metadata/`, `/subscriptions/`)
now return a Fabric-shaped JSON 404 and never fall through to the SPA
(`internal/server/portal.go`, regression-tested in `routing_test.go`). The
entra-emulator side is unfixed and is why the driver points
`-ResourceManagerUrl` at a tenant-prefixed path, which escapes that
emulator's SPA fallback and 404s as JSON.

Same shape as every other surface here — unit tests for the model, then a
**real client** driving it, because that is what has repeatedly found real
bugs (the ABFS `PUT`-vs-`PATCH` truncation, MSAL's discovery 404).

Microsoft publishes PowerShell drivers in `fabric-samples` that are exactly the
three flows worth proving:

* `DeploymentPipelines-DeployAll.ps1`
* `DeploymentPipelines-SelectiveDeploy.ps1`
* `DeploymentPipelines-AssignToNewDeploymentPipelineAndDeploy.ps1`

All three support **ServicePrincipal** auth — which is precisely the
entra-emulator client-credentials path already proven. They need `pwsh` in the
e2e container; no capacity, no cloud tenant.

The regression tests that must exist regardless of client:

1. Rename a paired item on either side, deploy, assert **overwrite** — not a
   duplicate. (The test the name-matching implementation fails.)
2. Deploy a Lakehouse, assert the target's OneLake is **empty**.
3. Item in target but not source, deploy, assert it **survives** — deployment
   is not a mirror.
4. Ambiguous pairing on assign → assignment reports failure, and deploy from
   that stage fails.
5. Selective deploy touches only the named items.
6. Non-adjacent stage deploy is rejected.

## Sequencing

D0 → D1 → D2 → D3. D1 before D2 is not negotiable: deployment without a
correct pairing table is the silently-wrong-in-production failure described
above. D3 is independent and can land any time.
