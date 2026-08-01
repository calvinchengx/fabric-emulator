# e2e: the advanced medallion track, executed

The CI harness for the steps numbered 20 and up in
[`examples/medallion`](../../examples/medallion/) — the second source system and
everything it forces.

**This directory contains no pipeline code and no stack definition.** It is two
files: a compose overlay that changes one command, and a runner. The example
owns the pipeline; [`e2e/medallion`](../medallion/) owns the stack.

```
docker compose \
  -f ../medallion/docker-compose.yml \
  -f docker-compose.advanced.yml up
```

## Why a sibling job rather than more steps in the basic one

`run_advanced.py` runs the whole basic pipeline first — the advanced track
continues where the tutorial stops, so it inherits its state. Folding it into
the existing job would have made the basic witness slower and less legible,
and the basic witness is the one [docs/28](../../docs/28-tutorial-end-to-end.md)
points readers at. Two jobs, each proving one narrative.

The cost is that the basic pipeline runs twice per push. The medallion job took
2m16s on a recent full run, so that is an acceptable trade for keeping the two
proofs separable.

## What the advanced steps prove

| # | Step | Assertion |
|---|---|---|
| 20 | Second source | Contoso Web gets its **own** Key Vault secret and its own `AzureKeyVaultReference` connection; the vendor refuses a wrong key; a listing spanning both connections leaks neither secret; three nested-JSON files land verbatim |
| 21 | Web bronze | Nested orders flatten to 6 line rows; the catalog and customer list land as Delta; the fixture's own arithmetic holds (1 cancelled order, 1 line pointing at a product that does not exist, 205.70 across 4 clean lines) — and the **overlap with POS is pinned**: 4 shared people, 2 web-only, 1 POS customer unmatchable because POS holds no email for him, 9 distinct people in total |

Step 21's overlap assertions are the point of A1. A2 has to resolve two customer
sets into one, and a resolution step is only meaningful if the answer was
written down before it ran.

## Run

```sh
python3 e2e/medallion-advanced/run.py
```

Same engine-choice notes as [the basic harness](../medallion/README.md#choosing-the-container-engine-apple-silicon-in-particular)
— only `mcr.microsoft.com/mssql/server` is amd64-locked.
