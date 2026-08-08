# 44 — Interaction surfaces: how a user touches this emulator, versus real Fabric

**Status: survey and doctrine; the lakehouse browser, notebook view and Jupyter
gaps are closed, the capture-diff one ranked.** This maps every way a
real Fabric user interacts with their workspace onto what this emulator offers
for the same need — and records *why* the mapping is shaped the way it is, so
the next "should the portal do X?" discussion starts from a position instead of
from scratch.

## The doctrine: bring your own UI

The emulator's UI strategy is three-pronged, and none of the prongs is "clone
the Fabric portal":

1. **Real client tools are the authoring and query surfaces.** VS Code (the
   pinned Fabric extension contract), Jupyter, SSMS/ADS over TDS, DuckDB,
   Power BI Desktop, fabric-cli. This is the same *ecosystem conformance*
   doctrine the parity map runs on: a real client working against the emulator
   is parity **evidence**; a UI we build is not, because both ends are ours.
2. **The Svelte portal is an operator console** — it shows what real Fabric
   *cannot* show (the controllable clock, fault injection, the flow/lineage
   graph, identities) plus read views of emulator state.
3. **Sidecars ship real UIs rather than reimplementing them.** OpenMetadata is
   the catalog UI, Airflow is the orchestration UI, kustainer is the KQL
   engine. A UI that already exists is attached, not rebuilt.

### The thin/thick line for portal surfaces

The portal has a DAX runner and a terminal profile, so "no interactive surfaces"
is not the rule. The rule is:

> A portal surface may be a **thin pane over a documented, emulated API**
> (`executeQueries`, TDS) or a **render of stored state**. It must not be a
> **client re-implementation with its own semantics** — because users would then
> develop against a path no real Fabric client takes, which is the
> green-locally-different-in-Fabric failure class this emulator exists to
> prevent.

A notebook *editor* is on the wrong side of that line, and
[14-real-compute.md](14-real-compute.md) records the decision: *"the Svelte
portal gets a read-only notebook view, not an editor — authoring belongs to real
tools."* A cell model, output rendering, interrupt, and ETag save round-trips
would be the largest un-checkable hand-maintained parity artifact in the
codebase — and this repo's guards (the generated engine matrix, the witness
checker) exist precisely because such artifacts rot.

## The map

| Fabric workspace UI surface | Emulator answer today | Parity position |
|---|---|---|
| Workspace / item browser, CRUD | Portal `Workspaces` view; fabric-cli; the REST surface | ≈ parity for observing and managing |
| **Notebook editor** (Monaco, cells, run) | No EDITOR in the portal, by recorded decision — but the `Notebooks` view renders the stored definition read-only and starts a `RunNotebook` job. **`make up-jupyter` ships a real JupyterLab** wired to the family; plus the VS Code extension contract and `.ipynb` + git/fabric-cicd sync | Deliberate: authoring belongs to real tools (docs/14 D3), and the tool is now *shipped* rather than assumed installed |
| **Lakehouse explorer** (Tables/Files tree, data preview) | Portal **`Lakehouses`** view: tables, files and a Delta preview, read-only. Plus the real clients reading OneLake directly: DuckDB, delta-rs, azcopy, ADLS SDKs; Spark SQL over Livy | ≈ parity for browsing and preview. Read-only by construction — stored state rendered back, on the right side of the line; authoring stays with the real tools |
| Warehouse SQL editor | TDS endpoint → SSMS, ADS, `sqlcmd`, dbt; `executeQueries` REST; `portal-terminal` (ttyd) profile | Adequate via real clients; portal `Warehouse` view is config-status only |
| Monitoring hub (runs, jobs) | Portal `Jobs`, `Operations` | ≈ parity |
| Lineage view | Portal `Flow` graph | Arguably **ahead** of Fabric — lineage edges are first-class and filmable (flow.gif) |
| Semantic model view | Portal `Models`: schema, measures, a DAX runner over `executeQueries` | ≈ parity for inspection; thin-pane rule satisfied |
| Deployment pipelines UI | API only (`e2e/deployment-pipelines` witnesses it) | Read-only portal view would be cheap; undemanded so far |
| Admin portal (tenant settings, domains, labels) | APIs only | Low demand; APIs are witnessed |
| OneLake catalog / data hub | OpenMetadata, `--profile governance` | There, opt-in, as a shipped real UI |
| Monitoring: capacity/billing | Out of scope (docs/03 non-goals) | — |

## Running Python, specifically

Four routes, all executing on the real engine, none of them a UI this repo owns:

| Route | What it is | Witness |
|---|---|---|
| Livy sessions | REPL over the emulator's native Livy surface, executing on Sail | `e2e/livy` |
| RunNotebook jobs | Notebook item (git / fabric-cicd / API) run as a job; the emulator drives the cells | `e2e/notebook-driven`, `e2e/notebook-run` |
| VS Code extension | The real extension's authoring protocol, pinned at 1.18.1, served through the `api.powerbi.com` alias | `e2e/vscode-extension` |
| `notebookutils` locally | The shim package, so notebook code runs unmodified in plain Jupyter/pytest | `e2e/notebookutils` |

The composed loop (docs/14): author in VS Code → sync via git/fabric-cicd →
execute on Sail with the default lakehouse mounted and the Environment item's
packages installed → Delta lands in OneLake → query over TDS/DuckDB → schedule
via the jobs API.

## Viewing tables, specifically

- **Warehouse**: any TDS client, or `executeQueries`.
- **Lakehouse**: DuckDB / delta-rs / ADLS SDKs against OneLake; Spark SQL over
  Livy; OpenMetadata for the catalog view. In-portal: the **`Lakehouses`** view
  lists tables and files and previews a table's schema and first-N rows, through
  the same reader the warehouse preview already used.

## On testing the VS Code surface

`e2e/vscode-extension` **replays the extension's captured route contract** — it
does not drive the extension's UI. That is the right witness for the emulator's
side of the boundary: the wire is what the emulator can break, and the UI is
Microsoft's.

Driving the real extension in CI is blocked twice over: the extension is
closed-source, and its MSAL auth targets `login.microsoftonline.com` with no
documented endpoint override. The hosts-redirect route
([05-tls-and-hosts.md](05-tls-and-hosts.md)) makes a *manual* smoke possible.
The scalable guard is **capture-diff**: capture a newer extension version's
traffic and diff it against the pinned contract, so protocol drift fails loudly.
Open question before building that: the original 1.18.1 capture tooling is not
in the repo, so reproducing the capture is the first step.

## Gaps, ranked

1. ~~**Lakehouse browser in the portal**~~ ✅ **Done** — the `Lakehouses` view.
   Tables are derived from OneLake *paths*, not directory rows, because delta-rs
   writes no directories; the file list is capped and the count is not.
2. ~~**Read-only notebook view + a run button**~~ ✅ **Done** — the `Notebooks`
   view. Cells are parsed server-side by `notebook.Parse`, the same call an
   engine's run makes; Run starts a real `RunNotebook` job through
   `startJob`, and the view says up front when no engine is attached to pick
   it up.
3. ~~**`--profile jupyter` sidecar**~~ ✅ **Done** — see below.
4. **Capture-diff for the extension contract** (M, after re-establishing the
   capture method).

## Closed: the `jupyter` profile

    make up-jupyter        # then http://localhost:8888

A real JupyterLab, attached rather than rebuilt — the same answer OpenMetadata
gives for the catalog UI and Airflow gives for orchestration. What makes it a
*Fabric* notebook rather than a generic one:

- `notebookutils` is on `PYTHONPATH`, importable exactly as in a Fabric
  notebook, wired through its documented contract
  (`python/notebookutils/_config.py`). Tenant, client id and secret default to
  entra-emulator's seeded identity, so only the endpoints are named.
- `SPARK_REMOTE` points at **Sail** — the same engine a `RunNotebook` job and a
  Livy session use, so a cell here and a cell in a job execute identically.
- `pyspark-client` is pinned to the **same 4.1.1 the agent uses**. A kernel on a
  different client than the engine is precisely the drift this profile exists to
  avoid, and the pin comment in `pyproject.toml` explains what breaks otherwise.
- `./notebooks` is a host mount, so what you author survives the container and
  is the same file `fabric-cicd` publishes and git syncs.

This closes the one place the bring-your-own-UI doctrine was genuinely weak: a
newcomer running `make up` could not touch a notebook without installing VS Code
or Jupyter first. It does **not** reverse the doctrine — the editor is still a
real tool, it is simply now shipped rather than assumed.

What stays out: a portal notebook editor, and real-extension UI automation in
CI — both for the reasons above.
