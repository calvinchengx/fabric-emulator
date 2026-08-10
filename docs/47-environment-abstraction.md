# 47 — What must not be in data-engineering code

**The rule.** The code is identical across emulator, DEV, QAT and PROD. Every
value that differs between them is resolved *outside* the code, by one
resolver, from one input.

The test is not "does it work in both places" — string-templating two branches
works. The test is: **can you `diff` the notebook, the pipeline definition and
the dbt project between two environments and get nothing back?** If a promotion
requires editing an artifact, the artifact carries an environment value and the
promotion is a rewrite.

This doc says what those values are. [21 — one toggle](21-real-fabric-toggle.md)
says how the resolver works; this says what user code is not allowed to contain.

## Three categories, not two

The usual framing is "abstract the environment-specific things, hardcode the
rest." That misses a third case, and the missing case is the one that corrupts
deployments silently.

| | what it is | rule |
|---|---|---|
| **Environment-bound** | differs per environment | resolve outside the code |
| **Environment-invariant** | the same everywhere *by the team's convention* | may live in code |
| **Environment-identical** | must be *byte-identical* across environments, and is neither free nor abstractable | preserve exactly; never regenerate |

The third category exists because Fabric has values that are neither yours to
choose nor safe to vary. The canonical one is **`logicalId`** in an item's
`.platform` file: it is the cross-workspace identifier linking an item in a
workspace to its representation in a Git branch, and Fabric's own documentation
says it is "essential not to change it in any way." Two workspaces synced from
one branch hold the *same* `logicalId` for the same item — that is the mechanism,
not a collision. Regenerate it per environment and Fabric stops recognising DEV's
item and PROD's item as the same item; promotion silently creates a duplicate
instead of updating.

Item **type** and the definition **part paths** (`notebook-content.py`,
`pipeline-content.json`) are the same kind of value: fixed by the platform, not
by you, and not parameters. They are Fabric's source format — see
[11 — testing with fabric-cicd](11-testing-with-fabric-cicd.md).

## Environment-bound: the list

### Fabric identity and topology

Workspace GUIDs · workspace display names · lakehouse GUIDs and display names ·
warehouse GUIDs and display names · item GUIDs (notebook, pipeline, semantic
model, eventhouse, environment) · Fabric Environment (Spark environment) id or
name · capacity id · connection ids and linked-service references · shortcut
targets (ADLS/S3 account, container, path) · OneLake `abfss://` URIs · domain and
workspace-folder assignment.

**Two clarifications that matter in practice.**

*Display names are environment values when they encode an environment.* The
toggle's contract is deliberately **name-based** — ids can never match across
targets, so user code holds names and the resolver maps them to GUIDs. That is
easy to misread as "names are safe to hardcode." They are not, when the name is
`MSF-DEV-DE-analytics`. Keep a **logical** name in code (`analytics`) and let
the resolver compose or look up the **physical** one. A name that contains an
environment is an environment value wearing a name's clothing.

*`abfss://` URIs are derived, not independent.* A OneLake URI is a function of
workspace + item GUIDs. Parameterise it separately and you have two sources of
truth that can disagree; the one that disagrees silently is the URI, because it
still resolves — to the wrong lakehouse. Abstract the GUIDs and **derive** the
URI.

### Environment identity

Environment name literals (`DEV`, `QAT`, `PROD`, or whatever the organisation
uses) · branch names that encode an environment · deployment stage names.

**`FABRIC_TARGET` is not an environment name, and the two axes are
orthogonal.** The target selects *which service and which kind of credential*
(`emulator` | `real`). The environment selects *which workspace, vault and data*
(`DEV` | `QAT` | `PROD` | whatever you call the local one). They vary
independently: `FABRIC_TARGET=real` with a DEV workspace is the normal case for
a developer working against the service, and `FABRIC_TARGET=emulator` with a
QAT-shaped configuration is how you rehearse a promotion offline.

Putting `emulator` in the same enum as `DEV`/`QAT`/`PROD` collapses two
questions into one and makes "run PROD's config against the emulator"
inexpressible — which is precisely the rehearsal the emulator exists to enable.
Keep them as two inputs.

### Secrets and auth

Key Vault URIs (bootstrap and secret vaults) · secret names · service principal
and application client ids · tenant id · managed identity and workspace identity
references · certificate names and thumbprints · any inline credential, token or
connection string.

### External systems

API base URLs and hostnames (sandbox vs production) · API endpoint paths that
differ per environment · SFTP, database hostnames, ports and database names ·
storage account and container names · notification and alert recipients.

### Operational

Schedules and trigger cadences · retention and purge windows · row limits and
sampling used only outside production · alerting thresholds · pool sizing and
concurrency.

## The bootstrap floor: you cannot reach zero

Abstraction has a floor, and pretending otherwise produces a resolver that
reads its own configuration from somewhere it also has to be told about.

Exactly two things must arrive from outside:

1. **Which environment am I?** One value. Not one per subsystem.
2. **A credential obtainable without a secret.** `DefaultAzureCredential` for
   the real target — env service-principal variables when set, otherwise the
   developer's `az login`, otherwise a managed identity. The emulator target
   uses the seeded development principal, which is public by construction.

Everything else — vault URI, workspace GUID, lakehouse GUID, connection id —
is *derivable* from those two. The discipline is not "no configuration"; it is
**one input, resolved once, in one place.**

This floor is why a platform must not require `AZURE_CLIENT_SECRET`. A Fabric
notebook has no client secret to give, so a platform that demands one cannot run
in the service it targets. `contoso-data-platform` shipped exactly that defect
while it restated the toggle's contract locally instead of consuming the
published package, and recorded the lesson in its own source: *a contract you
copy is a contract you get wrong.*

## Environment-invariant: what may stay in code

Medallion schema names (`bronze`, `silver`, `gold`) · logical table and column
names · data contract and model names · relative `Files/` paths inside a
lakehouse · business logic constants.

These are the organisation's choices and have nothing to do with the emulator.
Two cautions:

**They are invariant by convention, not by nature.** An organisation that names
schemas `bronze_dev` has made them environment-bound and must treat them so. The
convention is worth *keeping* precisely because it keeps them out of the
resolver.

**Contract and model *versions* are not invariant.** Names are; versions are the
one item in this list that routinely differs across environments, because that
is what a promotion *is* — DEV validates contract v3 while PROD still serves v2.
Treat the version as environment-bound during rollout, or the first schema change
turns an "invariant" into an outage.

## Where the resolver lives

`python/fabric-target/` (published as `fabric-target`), documented in
[21 — one toggle](21-real-fabric-toggle.md). Consumers install it rather than
reimplementing it — see the bootstrap-floor note above for what happens when
the contract is copied instead.

## Compliance today, measured

| | resolver | definitions in source format | verdict |
|---|---|---|---|
| `contoso-data-platform` | consumes published `fabric-target`; addresses by name; `DefaultAzureCredential` on the real target | publishes `notebook-content.py` via `updateDefinition`; carries `.platform` in `artifacts/pbip/` | **compliant** |
| `examples/fab-driven` | none | `definitions/silver.Notebook/notebook-content.py` + `.platform` | source format only |
| the four medallion examples | none | correct definition *envelope*, but definitions are inline Python strings | **not compliant** |

The medallion examples build a real definition envelope — `{"definition":
{"parts": [{path, payloadType: "InlineBase64", payload}]}}` with the correct
paths — and then pin themselves to the emulator: `examples/contoso-fixtures/
common.py` hardcodes the seeded `TENANT`, `CLIENT_ID` and `CLIENT_SECRET` and
defaults every endpoint to `https://localhost:*`. There is no flag; there is a
harness aimed at one target.

`examples/README.md` already names the inversion: the downstream consumer
demonstrates the toggle and the emulator's own examples do not, and *"that is
the wrong way round."* This doc is the standard those examples should be brought
to; it does not claim they meet it.

## Making it stick

Prose does not hold an invariant. Two mechanisms, neither built yet:

1. **A portability gate.** Fail any example that claims portability while
   naming a seeded credential or a `localhost` endpoint, and any item creation
   whose part paths are not the documented ones.
2. **An example under both targets.** `real-fabric.yml` currently runs the
   conformance suite and one notebook — neither is an example. Until a
   medallion example runs green under `FABRIC_TARGET=real`, "the same code runs
   against real Fabric" is witnessed for the *library*, not for the *pattern a
   user copies.*
