# 29 — T-SQL parity: Fabric's surface area vs the stand-in engine

**Status: T6, T7 and T8 shipped.** This doc maps Fabric's documented
T-SQL surface area against the SQL Server sidecar the emulator relays to,
classifies every divergence in **both** directions, and tracks the work to close
them. [16-warehouse-tds.md](16-warehouse-tds.md) describes the surface this
builds on (T1–T5).

- **T6 ✅** — nested CTEs are flattened on the wire, in batches and in RPC
  parameters, so dbt's `accepted_values` and `relationships` run unmodified.
- **T7 ✅** — `-tsql-strict` refuses the Class B constructs Fabric rejects that
  the sidecar would otherwise run. Off by default.
- **T8 ✅** — CTAS becomes `SELECT … INTO`, including inside the `EXEC('…')`
  dynamic SQL dbt actually ships.

**Class A is empty**: both real gaps are closed, and the one remaining entry was
found on measurement to have been misclassified. Class B is 10-of-15 refusable
behind `-tsql-strict`, each exception carrying its reason.

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

**Class B is the more dangerous class.** A local green build is exactly the
signal the emulator exists to provide; a Class B gap makes that signal a lie.
Class A is loud and annoying; Class B is quiet and expensive. Most of Class B
is now refusable with **`-tsql-strict`** (T7 below) — off by default, because
removing capability breaks working setups and that is the operator's call.

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

| Feature | Fabric | SQL Server 2022 | Evidence | Status |
|---|---|---|---|---|
| **Nested CTE** (`WITH` inside a CTE body) | supported | rejected, `Msg 156` | doc + **obs** | ✅ **closed (T6)** — flattened to sequential form on the wire, in batches and RPC parameters. Unblocked dbt's `accepted_values` + `relationships` |
| **CTAS** (`CREATE TABLE AS SELECT`) | supported | not a SQL Server construct (`SELECT … INTO` instead) | doc + **obs** | ✅ **closed (T8)** — rewritten to `SELECT … INTO`, including inside the `EXEC('…')` dbt actually ships. Unblocked `+materialized: table` |

### Class B — Fabric rejects, the sidecar accepts (silent divergence)

Refused by **`-tsql-strict`** (T7) where the ✅ column says so; off by default,
because refusing them removes capability that works today.

| Feature | Risk if silent | `-tsql-strict` |
|---|---|---|
| **Recursive CTE** | High — hierarchy queries pass locally, fail in Fabric | ✅ `recursive-cte` |
| **Triggers** | High | ✅ `triggers` |
| Enforced `PRIMARY KEY` / `UNIQUE` / `FK` | **High** — local constraint enforcement you won't get in Fabric | ✅ `enforced-constraint` |
| **Synonyms** | Medium | ✅ `synonyms` |
| `SET TRANSACTION ISOLATION LEVEL` | Medium — silently changes semantics | ✅ `set-isolation-level` |
| `SET ROWCOUNT` | Medium | ✅ `set-rowcount` |
| `IDENTITY` seed/increment, `IDENTITY_INSERT` | Medium | ✅ `identity-seed`, `identity-insert` |
| `ALTER TABLE ADD` identity, non-`BIGINT` identity | Medium | ⬜ needs column-type analysis |
| **Materialized (indexed) views** | Medium | ⬜ needs correlating `CREATE INDEX` with its view, across statements |
| `SELECT … FOR XML` | Low | ✅ `for-xml` |
| `CREATE USER` | Low | ✅ `create-user` |
| Multi-column statistics | Low | ✅ `multi-column-stats` |
| `PREDICT`, `sp_showspaceused` | Low | ✅ `predict`, `sp-showspaceused` |
| `FOR JSON` in a subquery | Low | ⬜ needs real parsing to tell from the legal last-operator form |
| Queries against system/user tables | Low | ⬜ not attempted |
| Vector data type | Low | n/a — **obs**: SQL Server 2022 rejects `vector(3)` with `Msg 2715, Cannot find data type vector`, so both engines lack it and it is not a divergence |

Every row's *Fabric* side is **doc** — stated in Microsoft's published surface
area — with one exception marked inline: the vector row's claim about **SQL
Server** is **obs**, measured against the engine, because it asserts something
about the sidecar rather than about Fabric.

What none of these rows had before T7 was a *witness* for the emulator's own
behaviour. `TestCheckStrictCorpus` now pins both what is refused and what must
be left alone, so the ✅ column is asserted rather than described.

**Class A is now empty.** Both entries are closed, and the third — `ALTER TABLE`
inside an explicit transaction — turned out not to belong here at all; see
Class C.

### Class C — agree (no action)

`MERGE` (GA in Fabric), session-scoped `#temp` tables, standard and sequential
CTEs, views, ordinary DML, `INFORMATION_SCHEMA` / `sys` catalog views.

**`ALTER TABLE` inside an explicit transaction — reclassified from Class A on
evidence.** The row claimed Fabric supported it while SQL Server was "more
restricted", tagged **inf**: inferred, never witnessed. Measured against SQL
Server 2022, the inference was wrong — `ALTER TABLE ADD` inside
`BEGIN TRANSACTION … COMMIT` succeeds, and after `ROLLBACK` the added column is
gone, so the engine has full transactional DDL. Fabric documents the same
support. The two agree; there was never a gap to close.

That is the **inf** tag doing its job: it marked a claim as untrusted, and the
first time anyone checked, it was false. Any remaining **inf** row here should
be read the same way — a hypothesis, not a finding.

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

- **T6g — RPC-carried statements. ✅ Done.** `internal/tds/rpc.go` walks an RPC
  parameter list, finds the parameter carrying the statement, rewrites it and
  re-encodes with corrected lengths — closing the gap T6a found and T6e could
  only name.

  **Written to a stricter rule than the rest of T6, because the blast radius is
  larger.** A SQLBatch is one length-prefixed string; an RPC is a parameter list
  where every element must be measured exactly to reach the next, and a wrong
  width silently mis-frames everything after it. So:

  - any parameter type this file does not model → give up, forward untouched
    (TEXT/NTEXT/IMAGE, XML, UDT, `SQL_VARIANT` are all unmodelled);
  - any inconsistency while walking → give up, forward untouched;
  - after rewriting, the result is **re-parsed and compared with the original**:
    every other parameter must be byte-identical and the rewritten one must
    decode to exactly the SQL intended. If not, the original is forwarded.

  The failure mode of a bug is therefore *"the rewrite doesn't happen"*, never
  *"a corrupted request reaches the engine"*. When the parameter list cannot be
  measured at all, T6e's named rejection remains as the fallback, so the user
  still learns the cause instead of meeting a bare `Msg 156`.

  A rewrite that outgrows the parameter's declared maximum widens it to
  `nvarchar(max)` (PLP) rather than truncating — though in practice flattening
  *shrinks* a statement, since it removes a `WITH` keyword and a paren pair.

  **Witnessed against the real ODBC driver**, with values fixed in advance:

  | Statement (parameterized → RPC `sp_prepexec`) | Result |
  |---|---|
  | nested CTE with a bound parameter | ✅ 20 |
  | three-level nesting with a bound parameter | ✅ 50 |
  | sequential CTE (must stay untouched) | ✅ 50 |
  | plain `SELECT` (must stay untouched) | ✅ 30 |

  Backed by a truncation sweep (every prefix of a valid message either fails to
  parse or round-trips exactly) and `FuzzParseRPC` — 1.8M executions clean on
  the invariant that anything the parser accepts re-serialises byte for byte.
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
- **T6c — the flattener. ✅ Done — refusing shadowed statements rather than
  renaming them.** `tsql.Flatten` hoists inner CTEs ahead of their parents,
  depth-first and innermost-first, so every definition precedes its first use
  once the levels collapse. Sibling order is preserved, so an inner CTE can
  still reference an earlier sibling of its parent. A statement with nothing to
  rewrite comes back **byte-identical** with `changed=false`, so the relay
  forwards the original untouched.

  **On the shadowing trap, the plan offered "rename … or refuse". It refuses,
  deliberately.** Renaming a CTE means rewriting every *reference* to it, and
  finding those references needs a real SQL parser, not a lexer — in
  `WITH cte1 AS (SELECT 1 AS cte1) SELECT cte1 FROM cte1` the same token is a
  CTE, a column, and a table reference. A token-level substitution cannot tell
  them apart, and rewriting the wrong one yields a statement that still executes
  and **returns different rows** — exactly what "never silently approximate"
  forbids. So shadowed statements return `ShadowedNameError`, naming the CTE and
  telling the user to rename the inner one. Detection normalises quoting and
  case, since `[A]`, `"a"` and `a` name the same CTE.

  **Witnessed against a real engine, not just asserted in unit tests.** Five
  statements — dbt's `accepted_values` shape, simple nesting, three-level
  nesting, sibling-order dependency, and one with a CTE look-alike inside a
  string literal — were run through the emulator against SQL Server in both
  forms:

  | Statement | Original (nested) | Flattened |
  |---|---|---|
  | dbt `accepted_values` | rejected, `Msg 156` | **✅ returns 1** (the one out-of-domain row) |
  | simple nesting | rejected, `Msg 156` | ✅ returns 42 |
  | three levels | rejected, `Msg 156` | ✅ returns 7 |
  | literal look-alike | rejected, `Msg 156` | ✅ returns 3 |
  | sibling order | rejected, `Msg 156` | ✅ returns 5 |

  Each expected value was fixed in advance, so this is **semantic** equivalence
  — the rewrite returns the answer the original was meant to produce — not
  merely "the engine accepted it". T6f makes this harness permanent.
- **T6d — enforce Fabric's own restrictions. ✅ Done.** Checked *before* any
  rewrite, so fixing a Class A gap cannot open a Class B one. A refused
  statement is returned untouched with a `RestrictionError` naming the rule.

  **The scope rule is the one T6c created, and the risk was real — measured,
  not assumed.** Fabric scopes a nested CTE to its immediate higher level, so
  Microsoft's own example fails there with `Msg 208`:

  ```sql
  WITH outer_1 AS (WITH inner_1_1 AS (…) SELECT …),
       outer_2 AS (WITH inner_2_1 AS (…) SELECT … FROM inner_1_1, inner_2_1 …)
  SELECT * FROM outer_2;   -- inner_1_1 belongs to outer_1's scope
  ```

  Flattened and run against a real SQL Server, that statement **succeeds,
  returning `a=1, b=2`**. So without this check the emulator would have quietly
  executed SQL a real warehouse rejects — precisely the silent Class B failure
  this document exists to prevent. It is now refused as
  `out-of-scope-reference`.

  | Rule | Enforced as |
  |---|---|
  | nested CTE only in a `SELECT` statement | `select-only` |
  | no `INSERT`/`UPDATE`/`DELETE`/`MERGE` in a nested definition | `no-dml-in-definition` |
  | no `OPTION` query hints in a nested definition | `no-query-hint` |
  | names unique within a nesting level | `duplicate-name` |
  | a nested CTE visible only to its level and its parent's body | `out-of-scope-reference` |
  | nesting depth ≤ 64 | `max-depth` |

  Note `duplicate-name` (invalid on Fabric too) is deliberately distinct from
  `ShadowedNameError` (valid on Fabric, merely unflattenable here) — they mean
  different things to whoever has to fix the SQL.

  **Deliberately not enforced, because the engines already agree:** a nested CTE
  in `CREATE VIEW`, and one in a *general* subquery (`SELECT * FROM (WITH …) x`),
  are both rejected by Fabric with `Msg 156` — and neither parses as a leading
  `WITH`, so it is forwarded untouched and SQL Server rejects it with the same
  `Msg 156`. Two engines, one error, nothing to add. **Not enforced, and why:**
  `AS OF` in a nested definition needs temporal-clause parsing this lexer does
  not do; recursive CTEs are a pre-existing Class B entry owned by T7, not
  something T6 introduced.

  **The one accepted trade:** the scope check considers only identifiers that
  name a CTE *somewhere in the statement*, so ordinary table and column names
  cannot trip it — but a column sharing a name with a CTE in an unrelated branch
  is a false positive. That costs a refused statement (loud, Class A) rather
  than a wrong answer (silent, Class B), which is the direction this document
  asks for.
- **T6e — re-encode, and connect it to the wire. ✅ Done.**
  `internal/tds/dialect.go` is the single place the emulator alters a client's
  SQL, and it implements the three outcomes this document demands:

  - **rewrite** — a nested CTE is flattened and re-encoded;
  - **reject** — anything Fabric itself refuses, or that cannot be flattened
    faithfully, is answered with a TDS error naming the limitation;
  - **forward** — everything else passes through **byte-identical**, including
    statements the parser fails on. "I don't understand this" must never become
    "I'll guess".

  `rewriteBatch` copies the ALL_HEADERS block verbatim (it describes the
  request, not the statement) and re-encodes the text as UTF-16LE; framing is
  left to `WriteMessage`, which already chunks, so a rewrite that grows past one
  packet goes out correctly (tested at 20 kB). Wired into **both** request paths
  — the byte-forwarding splice relay and the re-encode stub loop — so the two
  agree on what they accept.

  Per T6a, a nested CTE arriving inside `sp_prepexec`/`sp_executesql` is
  rejected by name rather than left to surface as a bare `Msg 156`. The
  statement text is recovered with the tracer's heuristic, which is *only* ever
  used to reject, and only when the recovered text genuinely parses as a
  nested-CTE statement — garbage does not parse, so a misread yields no
  rejection rather than a wrong one.

  **The witness: dbt's native builtins now pass.** Restoring `accepted_values`
  and `relationships` to the medallion project — the two tests this whole
  milestone chain exists for — and running the full e2e:

  ```
  accepted_values_dim_customer_country__…                     PASS
  relationships_fct_orders_customer_id__customer_id__ref_…    PASS
  Done. PASS=15 WARN=0 ERROR=0 SKIP=0 TOTAL=15
  ==> medallion pipeline complete: 9/9 steps passed
  ```

  Zero `Msg 156` anywhere in the session, and the DQ gate still bites when the
  data is poisoned (`PASS=11 ERROR=1`), so the tests are real rather than
  vacuously green. Adaptation #2 in `e2e/medallion/README.md` is now removable —
  left in place until **T6f** makes this harness permanent rather than a
  hand-run overlay.
- **T6f — witnesses. ✅ Done. The workaround is deleted, not merely unnecessary.**

  **The golden corpus** (`internal/tsql/golden_test.go`) states the contract in
  one table: every statement shape T6 claims to handle, with its outcome —
  `flatten` (with the exact expected SQL), `same` (forwarded byte-identical),
  `rule:X` (refused as a named Fabric restriction), `shadow`, or `parse`. It
  covers the dbt wrapper, three-level nesting, sibling dependencies, the
  `;WITH` idiom, quoted names with column lists, all five T6d refusals, both
  shadowing forms, and the four traps a regex falls into — a CTE look-alike
  inside a string literal, inside a comment, inside a quoted identifier, and a
  `WITH (NOLOCK)` table hint. Rewrites are asserted idempotent, and a companion
  test fails if the corpus ever stops covering an outcome.

  **The e2e now runs dbt's builtins unmodified.**
  `examples/medallion-pyspark/gold/models/schema.yml` uses `accepted_values` and
  `relationships` directly; the two CTE-free singular tests that stood in for
  them are **deleted** (`assert_no_negative_revenue.sql` stays — it is a genuine
  business rule, never a workaround). Adaptation #2 is gone from
  `e2e/medallion/README.md`.

  That is what makes the parity claim real: the statements that could not run
  here three milestones ago are now the ones CI runs on every push, with no
  emulator-shaped substitute in the project. If the rewriter regresses, the
  medallion job goes red.

#### Non-goals for T6

Recursive CTEs (Fabric rejects them too — Class B, so *reject*, don't
implement); a general T-SQL parser; rewriting anything other than nested CTEs;
CTAS (tracked separately below).

### T7 — Class B truthfulness. ✅ Done, behind a flag

`-tsql-strict` (`FABRIC_TSQL_STRICT`) refuses constructs real Fabric rejects
that the sidecar would happily run, so a locally green build means a
Fabric-green build. **Off by default**, because unlike all of T6 this *removes*
capability: SQL that works today starts failing. The default flips only once
the checks have been exercised widely enough to trust.

Checked *before* any rewrite — "would Fabric run this at all?" precedes "how do
we make the sidecar run it?" — and applied to statements carried as RPC
parameters as well as batches.

| Class B construct | Refused as |
|---|---|
| **Recursive CTE** (a CTE referencing itself) | `recursive-cte` |
| **Triggers** | `triggers` |
| **Synonyms** | `synonyms` |
| `CREATE USER` | `create-user` |
| `SET TRANSACTION ISOLATION LEVEL` | `set-isolation-level` |
| `SET ROWCOUNT` | `set-rowcount` |
| `SET IDENTITY_INSERT` | `identity-insert` |
| `SELECT … FOR XML` | `for-xml` |
| `IDENTITY(seed, increment)` | `identity-seed` |
| Enforced `PRIMARY KEY` / `UNIQUE` / `FOREIGN KEY` | `enforced-constraint` |
| Multi-column statistics | `multi-column-stats` |
| `PREDICT` | `predict` |
| `sp_showspaceused` | `sp-showspaceused` |

**Still not enforced, with the reason** — a lexer cannot see these, and
guessing would trade a silent Class B for a noisy false refusal:

- **indexed ("materialized") views** — needs correlating a `CREATE INDEX` with
  the view it targets, across statements;
- **`FOR JSON` in a subquery** — Fabric allows `FOR JSON` only as the last
  operator, which needs real parsing to tell from the legal form;
- **queries against system tables**, and the **vector type** — SQL Server 2022
  has no vector type either, so that row is not actually a divergence here.

The same false-positive trade as T6d applies, and is more acceptable here: an
operator who turned strict mode on did so precisely to be told about
divergence, so a loud refusal beats a silent one. What it costs is spelled out
rather than hidden — a column named `identity` or a CTE whose name also names a
column can be refused.

### T8 — CTAS. ✅ Done

`CREATE TABLE … AS SELECT` → `SELECT … INTO`, closing the last Class A gap.
`INTO` is spliced before the first `FROM` at parenthesis depth 0, so a subquery
in the select list or a leading CTE cannot capture it, and a query with no
`FROM` takes it at the end. Fabric's `WITH (DISTRIBUTION = …)` table options are
dropped — SQL Server has no equivalent, and they affect data distribution rather
than results, which is the one non-syntactic change here and why it is called
out.

**The plan was wrong about the input, and measuring caught it.** T8 assumed dbt
sends a bare CTAS. It does not: dbt-fabric ships its DDL as *dynamic SQL* —

```sql
EXEC('CREATE TABLE [db].[dbo].[x__dbt_temp]  AS SELECT * FROM …
    OPTION (LABEL = ''dbt-fabric-dw'');');
```

so the CTAS lives inside a string literal and a statement-level rewrite misses
it entirely. The first implementation did exactly that and the e2e still failed
with `Msg 156` — which is why the witness came before the claim. `Adapt` now
also rewrites inside `EXEC('…')` arguments, unescaping and re-escaping the
literal. Only the single-literal form is touched: `EXEC(@variable)` or a
concatenated expression is left alone, since its content is not knowable.

`OPTION (LABEL = …)` needed no handling — verified directly against SQL Server
2022, which accepts it.

**Witnessed:** `examples/medallion-pyspark` now uses `+materialized: table`, so CI
builds real tables through the rewrite — `PASS=13 ERROR=0`, 9/9 steps, DQ gate
still failing on poisoned data. Adaptation #1 is deleted from
`e2e/medallion/README.md`, as T6f deleted #2. **Both Class A gaps are now
closed.**

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
