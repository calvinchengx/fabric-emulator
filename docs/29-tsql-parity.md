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

- **T6a — reconnaissance. ✅ Done — GO, with a caveat that shapes the scope.**
  `tds.TraceFunc` (`internal/tds/trace.go`, enabled by `FABRIC_TDS_TRACE=1`)
  logs every client→server message. Measured against the real Microsoft ODBC
  Driver 18 and a full nine-step `e2e/medallion` run:

  | Statement shape | Message type | A SQLBatch-only rewriter sees it? |
  |---|---|---|
  | plain `SELECT` | `SQLBatch` | ✅ |
  | sequential CTE | `SQLBatch` | ✅ |
  | **nested CTE, no parameters** | **`SQLBatch`** | ✅ |
  | parameterized `SELECT` | RPC `sp_prepexec` | ❌ |
  | **parameterized nested CTE** | **RPC `sp_prepexec`** | ❌ |
  | driver metadata | RPC `[sys].sp_datatype_info_100` | n/a — carries no SQL |
  | transaction control | `0x0E` (TransactionManagerRequest) | n/a |

  **The finding: parameterization, not statement content, decides the path.**
  A nested CTE sent by `cursor.execute(sql)` arrives as `SQLBatch`; the *same*
  statement sent as `cursor.execute(sql, [param])` arrives as an RPC
  `sp_prepexec` with the text in a parameter —

  ```
  SQLBatch bytes=166 sql="with o as (with i as (select 1 probe_c) select * from i) select * from o"
  RPC proc=sp_prepexec bytes=256 text="with o as (with i as (select @P1 probe_f) select * from i) select * from o"
  ```

  **Why this is still a GO:** across a complete nine-step medallion run, dbt
  issued **73 SQLBatch messages and zero SQL-carrying RPCs** — every one of its
  28 RPCs was `sp_datatype_info_100`, a metadata call. dbt does not
  parameterize, so SQLBatch alone unblocks the driving use case.

  **What it changes:** RPC parsing moves from "possibly required" to "required
  for completeness, not for dbt." T6 ships SQLBatch-first, and per the
  rewrite-or-reject principle must *detect* a nested CTE arriving via
  `sp_prepexec`/`sp_executesql`/`sp_prepare` and answer with an error naming the
  limitation, rather than forwarding it to surface as a bare `Msg 156`. Full RPC
  rewriting is deferred to **T6g** (below).

  Two incidental findings, both of which constrain T6b:

  - **dbt prefixes every statement with a comment** —
    `/* {"app": "dbt", "dbt_version": "1.12.0", …} */` — so the tokeniser cannot
    assume `WITH` is the first token; leading comments must be skipped.
  - **RPC parameter text is not 2-byte aligned** within the message, so any
    UCS-2 decoding of it must scan both parities (learned by getting it wrong:
    the first trace showed `text="*"` for a statement that was plainly there).

- **T6g — RPC-carried statements (deferred).** Parse `sp_prepexec` /
  `sp_executesql` / `sp_prepare` parameters, rewrite the statement parameter,
  and re-encode with corrected lengths. Bounded but real work: parameters carry
  TYPE_INFO, and the length prefix must be recomputed. Only needed for clients
  that parameterize *and* use nested CTEs; until then T6 rejects that
  combination honestly.
- **T6b — the parser. ✅ Done.** `internal/tsql` — a lexer plus a CTE-list
  parser, protocol-free (no TDS imports) so it is testable on plain strings and
  reusable by T8. `Parse` returns `(nil, nil)` for a statement with no `WITH`
  prefix (the common case, "nothing to do"), a `*Statement` tree for one that
  has, and an **error rather than a partial parse** for anything malformed or
  unfamiliar — because the caller's correct response to "I don't fully
  understand this" is to forward it untouched.

  What the lexer had to get right, each of which defeats a regex: `'it''s'`,
  `[a]]b]`, `"x""y"`, `N'unicode'`, `--` comments, **nestable** `/* /* */ */`
  blocks, and parens inside any of them. Two properties are fuzzed
  (`FuzzTokenize`, ~14M executions clean): it never panics, and its tokens
  reconstruct the source byte for byte — the invariant that makes offset-based
  splicing safe.

  **Validated against the real thing.** Pointed at the eight statements dbt
  actually generated in the failing run (`TSQL_CORPUS=<dir> go test`), the
  parser flags `accepted_values` as nested and all seven `not_null`/`unique`
  tests as not — exactly matching which statements SQL Server rejected. Nesting
  detection is a perfect predictor of the observed failure, which is the
  precondition for rewriting only what needs rewriting.

  Deliberately *not* done: no understanding of SELECT bodies, which are carried
  as raw text. Also unhandled by design — a nested CTE in the *second* statement
  of a multi-statement batch, since only the leading statement is examined.
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

## Reproducing the measurements

`FABRIC_TDS_TRACE=1` logs one line per client→server TDS message — the
instrument every milestone above is measured with:

```sh
docker compose -f e2e/medallion/docker-compose.yml \
  -f <(echo 'services: {fabric-emulator: {environment: {FABRIC_TDS_TRACE: "1"}}}') up
```

It is a diagnostic, not a contract: the RPC text extraction is a
longest-printable-run heuristic over parameter bytes, deliberately not a
TYPE_INFO decoder, and nothing it produces is used to rewrite anything. Off
unless the variable is set (a nil `TraceFunc` costs one nil check per message).

## Risks and open questions

1. **Is rewriting SQL a line this project should cross?** Today the TDS layer
   relays T-SQL verbatim (one guard aside). T6 makes the emulator a *translator*.
   The mitigation is the rewrite-or-reject principle plus witnesses, but the
   architectural precedent is real and deliberate.
2. ~~**The RPC path (T6a) may dominate the work.**~~ **Settled by T6a:** it does
   not. dbt sends 100% SQLBatch; only *parameterized* statements take the RPC
   path, and that combination is deferred to T6g behind an honest rejection.
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
