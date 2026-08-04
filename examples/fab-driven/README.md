# Example: driven by Microsoft's own Fabric CLI

Everything on the control plane here is done by **`fab`**, Microsoft's Fabric
CLI, unmodified and unaware it is not talking to Azure. It creates the
workspace, uploads the vendor export into OneLake, imports real item
definitions, runs both items to completion, and lists what they produced.

That is a different claim from the one the other examples make. They drive the
emulator with `requests` — code written here, against an API we also wrote, so
agreement is partly guaranteed by construction. `fab` was written by Microsoft
against real Fabric. When it works unchanged, the emulator's control plane is
not merely self-consistent; it matches what Microsoft's own client expects.

`e2e/fabric-cli` has driven `fab` for a while, but only ever over **empty
items** — `fab mkdir` a Notebook with no cells, a DataPipeline with no
activities. Nothing could be run. This example adds the half that was missing:
definitions go up, jobs execute, data moves, and the results are read back.

## Run it

```sh
docker compose up -d
```

That is this directory's compose file, **not** the one at the repo root — see
[Why its own stack](#why-its-own-stack). Then:

```sh
uv sync --frozen
uv run python pipeline.py
```

Or step at a time, which is how the code is meant to be read:

```sh
uv run python provision.py
```

| Step | What drives it | What it proves |
|---|---|---|
| `provision.py` | `fab mkdir` | a workspace on a capacity, and a lakehouse |
| `land.py` | `fab cp` | Microsoft's own uploader writes to OneLake |
| `import_items.py` | `fab import` / `fab export` | definitions go up, and come back byte-identical |
| `run.py` | `fab job run` | the emulator executes a Copy; Spark executes a notebook |
| `readback.py` | `fab ls` + delta-rs | the catalogue and the bytes agree |

Nothing here needs an `/etc/hosts` entry, and nothing asks you to trust a
certificate.

## Why its own stack

`fab` hardcodes **three** hostnames — the MSAL authority
`login.microsoftonline.com`, the control plane `api.fabric.microsoft.com`, and
the OneLake data plane `onelake.dfs.fabric.microsoft.com` — and reaches all
three on **:443 over TLS with a trusted CA**. `FAB_API_ENDPOINT_*` overrides the
*hosts* but carries no port, so `https://localhost:9443`, the address every
other example uses, is not one `fab` can reach at all.

(It really is three. This example first failed with `CERTIFICATE_VERIFY_FAILED`
on the OneLake host while every control-plane command kept working.)

So `fab` runs **in a container on the compose network**, where those hostnames
are aliases for the emulator and its certificate can be read straight off the
socket. Your half — the independent Delta read in `readback.py` — runs on your
machine against `localhost:9443`, unchanged.

That split is the design, and [`fabctl.py`](fabctl.py) is the seam: one function
shells out to `docker compose run --rm fab …`. Pointing this at **real** Fabric
means deleting that function and calling `fab` directly. It is a visible seam on
purpose, so the diff is obvious rather than buried.

## The trap this example is built around

**`fab job run` exits 0 on a job that never succeeded.**

Measured, not assumed: a notebook was allowed to time out, `fab` cancelled it,
and the process returned 0. (`fab get` on a missing item *does* return 1, so the
exit code is worth checking everywhere else.)

An example that drove `fab job run` under `set -e`, or with `check=True`, would
report **success on every failed run** — a full row of green ticks proving
nothing. So job outcomes here are read back from
`fab job run-list --output_format json` and asserted on, and
[`test_fabctl.py`](test_fabctl.py) feeds those assertions a cancelled run, a
failed run, and an empty run list to prove each one goes red:

```sh
uv run pytest test_fabctl.py -q      # needs no stack running
```

## Two more traps, both found by running it

**`fab cp` cannot create its own destination directory.** It resolves the target
before copying, and a OneLake path that does not exist yet resolves as a *local*
path — so the error is `Source and destination must be of the same type`, which
reads like a bad argument rather than a missing folder. `land.py` runs
`fab mkdir` on the OneLake folder first.

**`docker compose run` can destroy the stack.** It starts a service's
dependencies and recreates any whose resolved config has drifted, and this
stack keeps its store in memory — so a version resolved one way in `up` and
another way in a later step silently replaced the emulator, and the workspace
created in step 1 was gone by step 4. Two guards: the pins live in a committed
[`.env`](.env) rather than your shell, and [`fabctl.py`](fabctl.py) passes
`--no-deps`.

## Two emulator bugs it found

Worth stating plainly, because it is the argument for the example:

- **OneLake silently truncated an oversized upload.** `fab cp` sends a whole
  file in a *single* append; the DFS handler read it through a bare
  `io.LimitReader` at 64 MiB, which discards the rest and reports success. Every
  other client this project drives chunks its uploads, so none had ever crossed
  the ceiling. It is now a 413 at the ADLS-documented 100 MiB, and a refused
  write stores nothing.
- **`mssparkutils` was never defined.** The notebook prelude patched
  `notebookutils` inside a `try: import` that always failed here, so the
  `except` silently did nothing — and any notebook written against real Fabric
  died on `NameError`. Both spellings are now bound rather than
  patched-if-present.

Neither was reachable from code written in this repository. Both surfaced the
first time Microsoft's own client did the ordinary thing.

## Why `definitions/` is templated

A pipeline definition names the lakehouse it reads and writes **by GUID**, and
those GUIDs do not exist until `provision.py` has run. So `definitions/` holds
templates with `{{WORKSPACE_ID}}` / `{{LAKEHOUSE_ID}}`, and `import_items.py`
renders them into `build/` before uploading.

This is not a concession to the emulator. Microsoft's own `fabric-cicd` carries
a `find_replace` parameter file for precisely this reason: the same definition
has to land in dev, test, and prod with different ids.

## What this example does NOT do

- **Gold, dbt, and the Warehouse.** `fab` has no hand in T-SQL modelling, and
  reproducing gold here would need ODBC Driver 18 and `dbt-fabric` to re-prove
  what [`../medallion-pyspark`](../medallion-pyspark/) already proves. bronze →
  silver is the whole of this example. A pleasant side effect: with no SQL
  Server sidecar, this stack runs natively on Apple silicon.
- **Orders.** The medallion examples carry a second, semi-structured feed. It
  would add a code path but no new evidence about `fab`.
- **Deployment pipelines.** `fab` 1.6.1 ships no deployment-pipeline verbs;
  [`e2e/fabric-cli`](../../e2e/fabric-cli/) already drives them through
  `fab api`.
- **`fab table load`.** The verb exists and calls the Lakehouse `loadTable` API,
  which the emulator does not implement. The Copy activity covers the same
  ground, and pretending otherwise would be the more interesting failure.

## Version sensitivity

`fab` is pinned to **1.6.1** in the [`Dockerfile`](Dockerfile), and the emulator
images are pinned in [`.env`](.env) — which compose reads automatically, so every
command resolves the same images regardless of your shell. `fab`'s verb
set and output shape move between releases, and `--output_format json` is what
keeps this example off scraping its aligned text tables. Treat a version bump as
a change that needs the example re-run.

The reasoning behind all of this — including what was measured before any of it
was written — is [`docs/34-fab-driven-example.md`](../../docs/34-fab-driven-example.md).
