# 34 — A `fab`-driven example: what was measured before it was built

**Status: BUILT and RUNNING — 5/5 steps, end to end.** Microsoft's own Fabric
CLI can not only create items in the emulator — it can upload real item
**definitions**, move 71 MB into OneLake, run **jobs** to completion, and read
back what those jobs produced. [examples/fab-driven](../examples/fab-driven/) is
what that verdict bought, and building it found **two emulator defects** that no
other client had ever exercised (below).

This document is the price tag, written before the work rather than after it —
the same shape as [33-pbix-tooling.md](33-pbix-tooling.md)'s Phase 0. The
question was narrow and load-bearing: **`e2e/fabric-cli` only ever creates empty
items.** If `fab import` did not work against the emulator, an example could add
nothing the suite did not already prove, and the right answer would have been to
write that sentence in `examples/README.md` and stop.

## What was measured

Real `fab` 1.6.1, real MSAL service-principal auth, the `e2e/fabric-cli` compose
stack, nothing stubbed:

| Check | Result |
|---|---|
| `fab import` a Notebook — create path (`POST items` with a definition, LRO) | ✅ |
| `fab export` the same item back | ✅ **byte-identical** round trip |
| `fab import` again — update path (`updateDefinition` LRO) | ✅ definition replaced |
| `fab import` a SemanticModel (`.platform` + `definition.pbism` + `model.bim`) | ✅ |
| `fab import` a DataPipeline, then `fab job run` it | ✅ **Completed**, visible in `fab job run-list` |

The fifth row is the one that changed the design. The scope had `fab` provision
and *our* code run the pipeline; in fact `fab` drives the run too, and the
example is stronger for it.

The one thing that does **not** work in that stack is a Notebook run — it parks
at `InProgress`, because `e2e/fabric-cli`'s compose has no Spark agent. That is
the honest behaviour when there is no engine (see
[14-real-compute.md](14-real-compute.md)), not a defect, and the example's own
compose brings Sail and the agent along.

## The trap, and it is a bad one

**`fab job run` exits 0 on a job that never succeeded.**

Measured directly: a notebook run was allowed to time out, `fab` cancelled it,
and the process returned **0**. For contrast, `fab get` on a missing item
returns **1** — so `fab` does use exit codes, just not to report job outcomes.

This matters more than it looks. An example that drives `fab` from a shell
script under `set -e`, or from Python with `check=True`, would report **success
on every failed run**. Every step would go green and the example would prove
nothing — which is precisely
[10-testing.md](10-testing.md)'s recurring failure: a check that cannot fail.

So `examples/fab-driven` never trusts the exit code of `fab job run`. It reads
`fab job run-status --output_format json` and asserts on the `status` field, and
`test_fabctl.py` feeds that assertion a `Cancelled` fixture to prove it goes red.

A smaller version of the same trap bit the measurement itself: the first
exit-code check read `$?` after `fab job run … | tail`, which is `tail`'s status,
not `fab`'s. It reported a confident 0 that meant nothing. The re-run dropped the
pipe.

## What building it then found

The check above priced the work. Building it found four more things, and two of
them were emulator defects that no client had exercised before — which is the
argument for the example in one paragraph.

### The emulator silently truncated an oversized upload

`fab cp` sends a whole file in a **single** `?action=append` — no chunking at
all. The DFS handler read its body through a bare `io.LimitReader` at 64 MiB,
which **discards the remainder and reports no error**, so a 71 MiB upload was
stored as a 64 MiB fragment and answered `202 Accepted`.

It surfaced only because `fab` then flushed at the real length and the position
check disagreed. **A client that never flushed would have had a quietly
shortened file and no signal at all.** Every other client this project has
driven — delta-rs, the Azure SDK, Hadoop's ABFS driver — chunks its uploads, so
none had ever crossed the ceiling.

Fixed as a CLASS, not as one site. The idiom appeared **nine times**, and only
this one had ever been reached:

| Where | What a truncated body became |
|---|---|
| OneLake DFS append | the file, stored short (the one `fab cp` hit) |
| OneLake DFS create-with-body | the file |
| OneLake Blob Put Blob / Put Block | the file |
| OneLake Blob block list | a mis-parsed commit, reported as bad XML |
| VS Code resource PUT | the file, **with nothing parsing it** |
| VS Code notebook content PUT | the notebook definition |
| Airflow DAG file PUT | the DAG |
| MLflow proxy / Kusto relay | a request or a result set, silently altered |
| Livy HC acquire | the body retained on the session |
| Shortcut read, Key Vault, Entra, Kusto responses | data served **as** the file |

Several of those "looked safe" because they parse the body afterwards, so a
truncation 400s. That is luck, not design: the caller is told their input is
malformed when it was merely too big, and the first site that stops parsing
inherits the silent version.

[`internal/httpx`](../internal/httpx/) now holds one `ReadBounded` that reads
`max+1`, so "too big" is detectable, and returns **no data** on refusal — handing
back the part that fitted is how the truncation grows back one caller at a time.
Ceilings are named constants, and `MaxDFSAppend` is 100 MiB to match ADLS Gen2.

The part that outlasts the nine fixes is
`TestNoHandlerReadsABodyThroughABareLimitReader`: it walks the source and fails
on the banned idiom, with an explicit `bounded-read-exempt:` marker for the two
places that genuinely discard what they read — and a second test pinning that
exemption list, so the escape hatch cannot quietly widen. Every site also has a
behavioural test asserting 413, and one asserting that a refused write leaves
**nothing** behind: a 413 with a fragment after it is still corruption, just
better-labelled.

### `mssparkutils` did not exist

The notebook prelude patched `notebookutils.notebook.exit` inside
`try: import notebookutils`, which on this engine always raised ImportError —
so the `except` silently did nothing and neither spelling was ever defined.
Any notebook written against real Fabric died on
`NameError: name 'mssparkutils' is not defined`.

This is the same class as a check that cannot fail: it looked like support and
provided none. Both names are now **bound** rather than patched-if-present, and
`TestNotebookPreludeBindsBothFabricSpellings` runs the prelude through a real
Python — asserting on the text of the prelude would have passed on the broken
version too.

### `fab` uses three hostnames, not two

The control plane is `api.fabric.microsoft.com`, but `fab cp` reaches OneLake on
`onelake.dfs.fabric.microsoft.com`. The example first failed with
`CERTIFICATE_VERIFY_FAILED` against a hostname nothing had aliased, while every
control-plane command kept working.

### Two ordering traps in `fab` itself

- **`fab cp` cannot create its destination directory.** It resolves the target
  first, and a OneLake path that does not exist yet resolves as a *local* path —
  so the error is `Source and destination must be of the same type`, which reads
  like a bad argument rather than a missing folder. `fab mkdir` on the OneLake
  path first.
- **`docker compose run` can destroy the stack.** It starts a service's
  dependencies and recreates any whose resolved config has drifted. With an
  in-memory store, that wipes the workspace mid-run — it turned step 4 into
  "The Workspace could not be found" after step 1 had created it. The example
  pins versions in a committed `.env` and passes `--no-deps`.

A note on method, because it cost an hour: three consecutive runs "still
failing" were all against a **stale image**. `docker compose ps` and
`docker inspect --format '{{.Image}}'` both reported the tag and id I expected.
The only check that settled it was a log line compiled into the binary. When a
fix appears not to work, verify the running code, not the tag over it.

## Why the example is shaped the way it is

`fab` hardcodes `https://api.fabric.microsoft.com` and the MSAL authority
`https://login.microsoftonline.com`, both on **:443 with a trusted CA**.
`FAB_API_ENDPOINT_FABRIC` overrides the *host* but carries no port, so
`https://localhost:9443` — the address every other example uses — is not
reachable by `fab` at all.

Three shapes were possible:

| | Shape | Verdict |
|---|---|---|
| A | Runs on the host like the other examples | needs `/etc/hosts` **plus** a CA in the reader's own trust store, with OS-specific instructions. Worst first run. |
| B | Everything inside compose | zero setup, but it is a fifth e2e suite wearing an example's clothes — nobody lifts it out and adapts it. |
| **C** | **`fab` in a container, the verification on the host** | **chosen** |

C puts the borrowed-authority tool where its TLS requirements are already
solved, and keeps the property [examples/README.md](../examples/README.md)
claims for an example: code you can copy out and change. The reader needs no
`/etc/hosts` entry and imports no certificate.

The cost is a wrapper: [`fabctl.py`](../examples/fab-driven/fabctl.py) shells out
to `docker compose run --rm fab …`. That is a real seam, and it is visible in
every step — which is better than hiding it, because a reader adapting this
against **real** Fabric deletes the wrapper and calls `fab` directly, and the
diff is obvious.

## What it does NOT cover

- **Gold / dbt / the Warehouse.** `fab` has no hand in T-SQL modelling, and
  reproducing it here would need ODBC Driver 18 and `dbt-fabric` to re-prove
  what [examples/medallion-pyspark](../examples/medallion-pyspark/) already
  proves. bronze → silver is the whole of this example.
- **Deployment pipelines.** `fab` 1.6.1 ships no deployment-pipeline verbs;
  `e2e/fabric-cli` already drives them through `fab api`, and repeating that
  here would add a second copy of the same evidence.

## Version sensitivity

Every assertion above is against **`fab` 1.6.1**, pinned in `pyproject.toml`'s
`fabric-cli` group. The CLI parses text output and its verb set moves between
releases — it gained no `deploy` verb through 1.6.x, and `--output_format json`
is what keeps this example off table-scraping. Treat a version bump as a change
that needs the example re-run, the same discipline `e2e/xmla` needs.
