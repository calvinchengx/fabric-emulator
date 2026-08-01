# 29 — T-SQL parity: Fabric's surface area vs the stand-in engine

**Status: plan.** This doc maps Fabric's documented T-SQL surface area against
the SQL Server sidecar the emulator relays to, classifies every divergence, and
sets out the work to close the one that currently blocks real tooling (nested
CTEs). Nothing here is implemented yet; [16-warehouse-tds.md](16-warehouse-tds.md)
describes what *is* (T1–T5).

## Why this doc exists

[16-warehouse-tds.md](16-warehouse-tds.md) settles the architecture: the
emulator owns the TDS protocol and FedAuth in pure Go, and relays T-SQL to a
**SQL Server 2022 sidecar** because no pure-Go T-SQL engine exists. That doc is
honest that the sidecar is "a vanilla T-SQL engine," not Fabric's MPP warehouse
— but it never says *where* the two differ, so every divergence has been
discovered the hard way, one broken e2e at a time (dbt's `table` materialization
in [`e2e/dbt-fabric`](../e2e/dbt-fabric/), then `accepted_values` and
`relationships` in [`e2e/medallion`](../e2e/medallion/)).

This doc replaces that discovery-by-accident with a map derived from Microsoft's
own documentation.

## The insight: the gap runs in two directions

The tempting mental model is "the sidecar is a subset of Fabric — some things
just don't work yet." **That is wrong, and the wrongness matters.** Fabric's
T-SQL is neither a subset nor a superset of SQL Server's. It *removes* things
SQL Server has, and *adds* things SQL Server lacks. So divergences fall into
three classes with very different consequences:

| Class | Meaning | Symptom | Who gets hurt |
|---|---|---|---|
| **A — emulator too strict** | Fabric supports it; the sidecar rejects it | Your SQL fails locally but would work in Fabric | Blocked locally; you write a workaround you didn't need |
| **B — emulator too permissive** | Fabric rejects it; the sidecar accepts it | Your SQL passes locally, then **fails in production** | Silent — discovered only on real Fabric |
| **C — agree** | Both behave the same | None | Nobody |

**Class B is the more dangerous class**, and it is invisible today. A local
green build is exactly the signal the emulator exists to provide; a Class B gap
makes that signal a lie. Class A is loud and annoying; Class B is quiet and
expensive.

The emulator's governing rule — *never fake results; either do it for real or
fail honestly* — has so far been applied to **engines**. This doc extends it to
**dialects**: a divergence the emulator knows about should be surfaced, not
silently passed through.

## Method

Every row below is derived from Microsoft's published surface area, not from
inference, and is tagged with how we know:

- **doc** — stated in Fabric documentation (cited).
- **obs** — observed empirically in this repo (an e2e or a captured error).
- **inf** — inferred from SQL Server behaviour; **needs a witness before being
  trusted**.

Primary sources: [T-SQL surface area in Fabric Data Warehouse][sa],
[Limitations of Fabric Data Warehouse][lim], [Nested CTE (Fabric)][nested],
[IDENTITY columns in Fabric Data Warehouse][ident].

## The parity map

### Class A — Fabric supports, the sidecar rejects

| Feature | Fabric | SQL Server 2022 | Evidence | Impact |
|---|---|---|---|---|
| **Nested CTE** (`WITH` inside a CTE body) | supported (preview) | rejected, error 156 | doc + **obs** | **Blocks dbt tests** — `accepted_values`, `relationships`, unit tests |
| **CTAS** (`CREATE TABLE AS SELECT`) | supported | not a SQL Server construct (`SELECT … INTO` instead) | doc + **obs** | Blocks dbt `table` materialization; forces `view` |
| `ALTER TABLE` inside an explicit transaction | supported | more restricted | doc / inf | Rare |

### Class B — Fabric rejects, the sidecar accepts (silent divergence)

| Feature | Fabric | SQL Server 2022 | Evidence | Risk |
|---|---|---|---|---|
| **Recursive CTE** | **not supported** | supported | doc | High — hierarchy queries pass locally, fail in Fabric |
| **Triggers** | **not supported** | supported | doc | High |
| **Materialized views** | not supported | indexed views exist | doc | Medium |
| **Synonyms** | not supported | supported | doc | Medium |
| `SET TRANSACTION ISOLATION LEVEL` | not supported | supported | doc | Medium — silently changes semantics |
| `SET ROWCOUNT` | not supported | supported | doc | Medium |
| `SELECT … FOR XML` | not supported | supported | doc | Low |
| `FOR JSON` in a subquery | must be the last operator | unrestricted | doc | Low |
| `CREATE USER` | not supported | supported | doc | Low |
| Enforced `PRIMARY KEY` / `UNIQUE` / `FK` | only with `NOT ENFORCED` | fully enforced | doc | **High** — local constraint enforcement you won't get in Fabric |
| `IDENTITY` seed/increment, `IDENTITY_INSERT`, `ALTER TABLE ADD` identity, non-`BIGINT` identity | not supported | supported | doc | Medium |
| Queries against system/user tables, multi-column stats, `PREDICT`, `sp_showspaceused`, vector type | not supported | varies | doc | Low |

### Class C — agree (no action)

`MERGE` (GA in Fabric), session-scoped `#temp` tables, standard and sequential
CTEs, views, ordinary DML, `INFORMATION_SCHEMA` / `sys` catalog views.

> **Lakehouse SQL analytics endpoint** is read-only in both Fabric and the
> emulator (`INSERT`/`UPDATE`/`DELETE` are Warehouse-only) — already enforced by
> the read-only guard in `internal/tds/splice.go`. That is existing Class C
> behaviour and the precedent for everything below.

## The plan

### Principle: rewrite or reject — never silently approximate

Translating a dialect the stand-in engine lacks is **not** faking a result: the
rows still come from a real engine running real SQL. But a rewrite that changes
semantics *is* faking, and worse than failing. So:

1. If a construct can be rewritten **provably semantics-preserving**, rewrite it.
2. If it cannot, **reject it with a clear error naming the limitation** — never
   pass through a form that quietly means something else.
3. Where Fabric itself rejects something (Class B), prefer rejecting too, so the
   local build tells the truth about production.

### T6 — nested CTE support (the Class A blocker)

**Goal:** `WITH outer AS (WITH inner AS (…) SELECT …) SELECT …` executes
through the emulator against the sidecar, so dbt's `accepted_values`,
`relationships`, and unit tests run unmodified — removing adaptation #2 from
[`e2e/medallion/README.md`](../e2e/medallion/README.md).

**Approach:** flatten nested CTEs into the sequential form SQL Server accepts,
in the TDS layer, before forwarding to the backend.

```sql
-- what the client sends (Fabric-legal, SQL Server-illegal)
WITH outer_cte AS ( WITH inner_cte AS (SELECT * FROM t1) SELECT * FROM inner_cte )
SELECT * FROM outer_cte;

-- what the backend should receive (semantically identical, SQL Server-legal)
WITH inner_cte AS (SELECT * FROM t1),
     outer_cte AS (SELECT * FROM inner_cte)
SELECT * FROM outer_cte;
```

**Where it hooks.** `spliceSession` (`internal/tds/splice.go`) already
intercepts `PktSQLBatch`, extracts the text with `sqlBatchQuery`, and classifies
it with `isWriteStatement` for the read-only guard. T6 extends that one
interception point from *classify-and-reject* to *classify, rewrite, re-encode,
forward*.

#### Milestones

- **T6a — reconnaissance (do this first; it can invalidate the rest).**
  Instrument the relay and record which TDS packet type actually carries the
  CTE-bearing statements for each client. `PktRPC` (0x03) is *defined* in
  `internal/tds/packet.go` but **handled nowhere** — if pyodbc/dbt send queries
  as `sp_executesql` RPC with the SQL as an nvarchar parameter, a SQLBatch-only
  rewriter silently misses them. Deliverable: a table of client → packet type,
  and a go/no-go on whether RPC parsing is in scope.
- **T6b — the parser.** A nested-CTE-aware scanner over the `WITH` prefix only:
  find CTE definitions, their nesting, and their bodies. It must be a real
  tokeniser (string literals, `[bracketed]` and `"quoted"` identifiers, `--` and
  `/* */` comments, nested parens) — a regex will corrupt SQL containing the
  word `with` in a string. It does **not** need to parse SELECT bodies.
- **T6c — the flattener, with alpha-renaming.** Hoist inner CTEs ahead of their
  parents in dependency order. **Name shadowing is the correctness trap**:
  Fabric explicitly permits reusing a CTE name at different nesting levels, and
  its own documentation demonstrates it —

  ```sql
  WITH cte1 AS ( WITH inner_1 AS (…) SELECT … ),
       cte2 AS ( WITH cte1 AS (…) SELECT * FROM cte1 )  -- a DIFFERENT cte1
  ```

  Naive hoisting collides these two and **silently returns wrong rows**. The
  flattener must rename shadowed CTEs to fresh names and rewrite references
  within their scope, or refuse the statement.
- **T6d — enforce Fabric's own restrictions.** Nested CTEs are `SELECT`-only; no
  `OPTION` hints in a nested definition; not usable in `CREATE VIEW`; permitted
  in a CTE subquery definition but **not** a general subquery; no `AS OF`; names
  unique per level; visible only to the immediate higher level. Accepting these
  would turn a fixed Class A gap into a *new Class B gap* — so reject them with
  the same error text Fabric gives (`Msg 156`) where it applies.
- **T6e — re-encode.** Rebuild the SQLBatch payload: preserve the ALL_HEADERS
  block verbatim, re-encode the rewritten text as UTF-16LE, and recompute packet
  framing. Oversized batches must chunk correctly.
- **T6f — witnesses.** Unit tests over a golden corpus of nested-CTE statements
  (including the shadowing case, comment/literal traps, and every rejection in
  T6d), plus a gated e2e running the *unmodified* dbt builtins end to end. The
  parity claim is only real when `accepted_values` passes with no singular-test
  substitute.

#### Non-goals for T6

Recursive CTEs (Fabric rejects them too — Class B, so *reject*, don't
implement); a general T-SQL parser; rewriting anything other than nested CTEs;
CTAS (tracked separately below).

### T7 — Class B truthfulness (proposed, lower priority)

Reject, at the TDS layer, constructs Fabric documents as unsupported —
recursive CTEs, triggers, `SET TRANSACTION ISOLATION LEVEL`, `SET ROWCOUNT`,
enforced constraints — so a locally green build means a Fabric-green build.
This is *removing* capability the sidecar happens to have, which is the right
direction for an emulator but will break anyone relying on it, so it belongs
behind a flag (`-tsql-strict`) with a documented default before it becomes the
default.

### T8 — CTAS (proposed)

Rewrite Fabric/Synapse `CREATE TABLE AS SELECT` into SQL Server `SELECT … INTO`,
restoring dbt's `table` materialization and removing adaptation #1. Mechanically
simpler than T6 but shares the tokeniser, so it should follow it.

## Risks and open questions

1. **Is rewriting SQL a line this project should cross?** Today the TDS layer
   relays T-SQL verbatim (one guard aside). T6 makes the emulator a *translator*.
   The mitigation is the rewrite-or-reject principle plus witnesses, but the
   architectural precedent is real and deliberate.
2. **The RPC path (T6a) may dominate the work.** If clients send SQL as
   `sp_executesql`, scope grows substantially.
3. **Nested CTE is a preview feature in Fabric.** Its semantics may shift; the
   witnesses must be re-checked against the GA behaviour.
4. **dbt-fabric is itself broken here.** [microsoft/dbt-fabric#318][i318]
   ("Nested CTEs fail on dbt-fabric versions >= 1.9.2") is **open**. So even
   after T6, the adapter may fail against real Fabric for its own reasons — the
   emulator would then be *more* capable than the real-world toolchain. Worth
   stating plainly in any parity claim.
5. **Class B has no witnesses at all today.** Every `inf` row above is a guess
   until tested against both engines.

## Where this shows up

- [16-warehouse-tds.md](16-warehouse-tds.md) — the architecture this extends.
- [`e2e/medallion/README.md`](../e2e/medallion/README.md) adaptations #1 and #2
  — the two Class A gaps, to be deleted as T6/T8 land.
- [parity.md](parity.md) — the claim/witness map; T6 needs an entry there.

[sa]: https://learn.microsoft.com/en-us/fabric/data-warehouse/tsql-surface-area
[lim]: https://learn.microsoft.com/en-us/fabric/data-warehouse/limitations
[nested]: https://learn.microsoft.com/en-us/sql/t-sql/queries/nested-common-table-expression?view=fabric&preserve-view=true
[ident]: https://learn.microsoft.com/en-us/fabric/data-warehouse/identity
[i318]: https://github.com/microsoft/dbt-fabric/issues/318
