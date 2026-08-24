# `notebookutils` — Microsoft's own stub package

Golden reference for **Axis A** of [docs/56](../../docs/56-notebook-capability-parity-plan.md):
what `notebookutils.*` exposes, and with which parameters. Copied in full
(MIT, 12 KB).

| Field | Value |
|---|---|
| **Upstream** | `dummy-notebookutils` on PyPI — https://pypi.org/project/dummy-notebookutils/ |
| **Pinned revision** | `1.6.3` (uploaded 2026-01-22) |
| **Retrieved** | 2026-08-24 |
| **Wheel** | https://files.pythonhosted.org/packages/5d/c1/2321118a61728a9debe7bf423e29c4854c9bfedcbd966805ebb9cfd5a62e/dummy_notebookutils-1.6.3-py3-none-any.whl |
| **License** | MIT — Microsoft Corporation. Text in [`LICENSE`](LICENSE) |
| **Used by** | `scripts/check_notebookutils_surface.py`, which checks every member of every Fabric-documented module against `python/notebookutils/` |
| **Refresh** | `python3 scripts/vendor_notebookutils_stubs.py --version <v> --today <YYYY-MM-DD>` — re-fetches, re-hashes and rewrites the table below |

## Why a stub package is the right oracle

The package has no functionality: every body is `pass` or a bare return. That
is exactly what makes it useful. Axis A is a question about the **surface** —
which members exist and what they are called — and this is Microsoft stating
that surface in a machine-readable form, shipped so notebook code can be
developed off-cluster. The alternative, which this replaces, was transcribing
signatures from Learn pages by hand: a claim about Microsoft's API with a
person in the middle, covering one module of nine, and unable to notice when
the surface moved.

## What it is NOT, and why the checker carries a second source

It is Synapse-lineage. Its own homepage is `Azure/azure-synapse-analytics` and
its summary says "synapse mssparkutils", so it is **broader than Fabric**:
`conf`, `connections`, `data` and `fabricClient` are absent from
[Fabric's module list](https://learn.microsoft.com/en-us/fabric/data-engineering/notebook-utilities),
and that page says in its own Known issues that `fabricClient` and `PBIClient`
"aren't supported yet".

So this pin is the oracle for **signatures**, and Fabric's documentation stays
the oracle for **scope**. `check_notebookutils_surface.py` holds both and
refuses to guess: a module that appears here and is classified in neither list
fails the build rather than being silently treated as in or out.

The legacy `notebookutils/mssparkutils/` namespace is vendored because it ships
in the wheel, and is deliberately **not** checked: Fabric's page says the
namespace is backward-compatible and "will be retired in the future", so
holding our shim to it would pin us to a surface Microsoft is removing.

## Integrity

<!-- integrity:begin -->
| File | sha256 | Bytes |
|---|---|---|
| `LICENSE` | `903df5512f7d02609fed0c780a9b704f5a3eeb6e4d84ebe42a29845c81899a3c` | 1093 |
| `notebookutils/__init__.py` | `22551788d59ff83b15c88748b1c2db16af02201ae9c2c297b464714e257c6146` | 358 |
| `notebookutils/conf.py` | `947834edb32bb87173ac7bc14df1bfdbff11e81c9c8cfc15442dc7bfa9866fcc` | 386 |
| `notebookutils/connections.py` | `6e12c665118a89b1641cb94fbb84938282dc9632db9328e7900247fbb381248d` | 756 |
| `notebookutils/credentials.py` | `00ecdad30a175e9f91248b3aee2c2f4310725e1c134889677e87bfecb9c31dbb` | 1239 |
| `notebookutils/data.py` | `1d70e04b82f838aaef435a160dcb06da44bf2d61f106ff6d0bbcaf44bd3576e1` | 218 |
| `notebookutils/fabricClient.py` | `9261ff2a1b700173d6958e7b853c2e1e13d4f06dc702e16567368d973bcf9587` | 334 |
| `notebookutils/fs.py` | `3015e2ee490d72405efcd18d95b9dd84c7d708436b0764158854bed186c39834` | 1097 |
| `notebookutils/lakehouse.py` | `73026baed61c8f41fd6e6882942382be6409888969f1b047ddfde1a14af436a5` | 695 |
| `notebookutils/mssparkutils/__init__.py` | `5b548feb632e124bf95437e20384a205930de55a3e192ec52bb011d84b4c6432` | 519 |
| `notebookutils/mssparkutils/credentials.py` | `ff7e3a0e2604d395b0901c270005c471c3efecc7a99244bc6f32ec07cfbda3de` | 1273 |
| `notebookutils/mssparkutils/env.py` | `a33fd7c99173be1b4ee9d9934ddf10912e053d1b041019e2e4e7151a20e0c589` | 258 |
| `notebookutils/mssparkutils/fs.py` | `d420e52ab17691c30f5f9470f4a63f9914ca91373669fec77c7b57d7bee6c31d` | 1060 |
| `notebookutils/mssparkutils/lakehouse.py` | `460ea1b4723b8877bba2d5d4e215c90e22870f5ba0a57f9187566cff40d9c538` | 348 |
| `notebookutils/mssparkutils/notebook.py` | `5ad84fc0912b5008d9a93b4d3397996b14864dda862f88cf69531e06370ff6d5` | 289 |
| `notebookutils/mssparkutils/runtime.py` | `f1874b50df6a519a17f1c94aab94a1914340ba6e5db0d3ec5822ffa6fde5f9a5` | 141 |
| `notebookutils/mssparkutils/session.py` | `ca6f1d52784ff5c594ae76ee5b7b133d0785860c36496a2e3d93a06e038cfc2c` | 59 |
| `notebookutils/notebook.py` | `5d954cd9b26038c9a75dd5c434c6a984fb4d41376cb49a5a83e541e777f18fa1` | 1006 |
| `notebookutils/runtime.py` | `2d42889887200e50c6d5e5eb379486eab1f51e4896b36b76bfccbfac15fd65f3` | 101 |
| `notebookutils/session.py` | `2bcaaa9aa3607f75ef064d6b2821e5275351462aec4c0348716a0b9ce6f0a80c` | 113 |
| `notebookutils/udf.py` | `4eeb11babf94007fe634cc70ecd2aaadf207175737c1a324d4f75dcab1fe196e` | 933 |
| `notebookutils/variableLibrary.py` | `29d6b0f6573b4332889948c0e2a84546e89dc36aa051a7abde18b978313e45aa` | 807 |
<!-- integrity:end -->
