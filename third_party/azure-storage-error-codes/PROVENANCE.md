# `StorageErrorCode` — Microsoft's own enumeration of storage error codes

Second source for **contract 8** (*refusal fidelity*) in
[docs/38](../../docs/38-framework-conformance.md): the code a caller branches on
when an operation is refused. Vendored so the check can read it with the standard
library alone, the way every other guard in `scripts/` runs.

| Field | Value |
|---|---|
| **Upstream** | `azure-storage-blob` on PyPI — https://pypi.org/project/azure-storage-blob/ |
| **Pinned revision** | `12.30.1` |
| **Retrieved** | 2026-09-01 |
| **Path in wheel** | `azure/storage/blob/_shared/models.py` |
| **License** | MIT — Microsoft Corporation. Text in [`LICENSE`](LICENSE) |
| **Used by** | `scripts/check_refusal_expectations.py`, which holds `REFUSAL_EXPECTATIONS` against the codes this file enumerates |
| **Refresh** | re-copy `_shared/models.py` from the pinned wheel and update the hash below |

## Why this file rather than the installed package

The guards in this repository run under `$(PY)`, which is
`uv run --frozen --no-sync python` or a bare interpreter — **standard library
only**. Importing `azure.storage.blob` to read one enum would make an offline
invariant depend on a synced environment, and the first CI job without `uv` on
PATH would fail on the guard rather than on the thing it guards. That is exactly
what happened when this check was first written to import the SDK.

So the file is vendored and parsed with `ast`, which is also what makes the
pin auditable: the hash below says which bytes were read.

## Why an SDK file is the right oracle, and what it is not

`StorageErrorCode` is Microsoft stating, in a machine-readable form, the set of
codes its storage services return — 165 of them, including `DirectoryNotEmpty`.
Our expectations are a hand transcription of the same contract, and a
transcription with one source cannot be wrong out loud.

**It enumerates codes; it does not say which operation returns which.** That
mapping stays ours, and stays graded by the live probe. This pin answers a
narrower question — *is this code one Microsoft actually defines* — which is the
half that goes stale silently when a code is renamed or retired.

**Azurite would have been the better witness** and cannot serve: it implements
Blob/Queue/Table and **not** ADLS Gen2, as `e2e/azurite-shortcut/docker-compose.yml`
states in its own header, so a DFS-specific refusal is precisely what it cannot
answer.

## Integrity

<!-- integrity:begin -->
| File | sha256 | Bytes |
|---|---|---|
| `LICENSE` | `fd532481d828e13a0b13ccb598e02338a3617740675a862ee6bdc1541b68e93d` | 1073 |
| `models.py` | `98f0266a18ee41cdc58281682e3c357a5d9e61a1ce782662bdc8153c0157a898` | 26101 |
<!-- integrity:end -->
