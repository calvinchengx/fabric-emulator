# 19 — Semantic-model / DAX engine plan

Turn Power BI from 🔴 (item management only) into a real query engine: the
`executeQueries` REST contract backed by a bounded, real DAX evaluator, so
**Great Expectations can validate semantic-model data** against the emulator
([e2e/great-expectations](../e2e/great-expectations/)).

**What this reaches in SemPy, measured against the sempy 0.14.2 wheel** — not
against its documentation, which sent two sessions the wrong way in *opposite*
directions before anyone grepped the package:

Established with `inspect.signature` / `inspect.getsourcelines`, because reading
line ranges got this wrong three times (below):

- **`evaluate_dax` (`_flat.py:1036`) is XMLA, unconditionally.** It takes no
  `use_xmla` parameter at all, and its body is a bare
  `return DatasetXmlaClient(...).evaluate_dax(...)` — no REST branch. This is
  the function that runs arbitrary DAX, so **`executeQueries` does not serve
  it.**
- **`evaluate_measure` (`_flat.py:945`) defaults to REST `executeQueries`** —
  it *does* take `use_xmla: bool = False`, and escalates to XMLA only on
  `use_xmla=True`, a readwrite connection, or `num_rows > 30000`
  (`_client/_pbi_rest_api.py:274` builds the `executeQueries` path). This
  engine serves that path today.
- `list_columns` / `list_hierarchies` / `list_partitions` /
  `list_relationships` use **XMLA `$SYSTEM.TMSCHEMA_*`**; `list_measures` and
  `list_tables` instead read **TOM** (`Microsoft.AnalysisServices.Tabular`)
  objects. Two mechanisms, not one.
- `INFO.*` appears **nowhere** in the wheel (0 hits) — implementing it over
  `executeQueries` would have moved SemPy not one inch.

**Why the precision:** this single fact flipped three times across three
sessions, each reversal held confidently and argued from evidence. The first
two were read out of documentation; the third came from citing `_flat.py:954`
without establishing which `def` encloses it — it is inside `evaluate_measure`,
not `evaluate_dax`. **Verify the proposition, not the citation:** a line can be
exactly where someone says and still belong to another function.

Grounded in the golden references pinned first:
[18-semantic-model-references.md](18-semantic-model-references.md),
[third_party/powerbi-rest-swagger](../third_party/powerbi-rest-swagger/) (the
executeQueries OpenAPI), and [e2e/semantic-model](../e2e/semantic-model/) (the
golden model + hand-computed DAX oracle).

**Why executeQueries was built first:** it is HTTP+JSON with a vendored OpenAPI
*and* a live oracle, so it was the cheapest real query surface to stand up.

That is the *only* claim this plan makes for it. It does **not** rest on XMLA
being infeasible: [`e2e/xmla`](../e2e/xmla/) measures Microsoft's own ADOMD.NET
running on Linux (.NET 8, in a container), connecting to a host we name via
`powerbi://<host>:<port>`, trusting a self-signed CA, and taking a bearer from
the connection string. XMLA is deferred on **cost, not feasibility** — see
[32-xmla-plan.md](32-xmla-plan.md) for the measured position and
[18-semantic-model-references.md](18-semantic-model-references.md) for the specs.

## Phases

Critical path to green: **A → C → D → E**. F is the tutorial's actual subject.

### A — TMSL model parsing (pure Go)
- [x] `internal/semanticmodel` parses `model.bim` (TMSL) → tables, columns,
      measures (DAX expr strings), relationships; loads it from the item's
      `model.bim` definition part.
- [x] Unit-tested against `e2e/semantic-model/fixtures/retail.bim`.

### B — table data binding
- [x] Engine reads an optional `data.json` definition part (import rows)
      alongside `model.bim`; rows addressable per table.
- [x] Unit-tested against `fixtures/seed_data.json`.
- [x] **Direct Lake:** compatibility level 1604 TMSL partitions with entity
      sources resolve their shared `AzureStorage.DataLake` expression to a
      Lakehouse and read its current Delta snapshot from OneLake. `sourceColumn`
      maps physical Delta fields to model columns; source-workspace RBAC is
      enforced. A later Delta commit is visible on the next DAX query without an
      import refresh (`e2e/data-science-loop`).

### C — the DAX evaluator (core)
- [x] Tokenizer + parser for the subset: `EVALUATE`, `SUMMARIZECOLUMNS`, table /
      column / measure refs, function calls, string literals.
- [x] Evaluation: filter context, relationship traversal (`Sales`→`Time`/`Store`),
      measure expansion, `SUM`, `DIVIDE` (blank on ÷0), `COUNTROWS`, `IF`,
      `EVALUATE <table>`, `SUMMARIZECOLUMNS(cols…, "name", expr)`.
- [x] Infix operators `+ - * / &` and comparisons, with DAX precedence and
      parentheses (issue #42). Stored measures using them are covered by the
      `Operator Measure Asset` golden — publication alone never reads the DAX,
      so only a query that names the measure is evidence it works.
- [x] Unit-tested against `fixtures/golden_queries.json` (the DAX oracle),
      order-insensitive.

### D — executeQueries REST endpoint
- [x] Routes per the vendored swagger: `POST /v1.0/myorg/datasets/{datasetId}/
      executeQueries` + the `/groups/{groupId}/…` variant.
- [x] Power BI audience (`https://analysis.windows.net/powerbi/api`) validator;
      Viewer RBAC; alias `api.powerbi.com`.
- [x] `datasetId` → SemanticModel item → parse + evaluate → executeQueries JSON
      (`Table[Col]` / `[Measure]` keys, `{results:[{tables:[{rows}]}]}`).
- [x] Handler unit tests + a server e2e: golden queries; bad-DAX error shape; unknown dataset
      404; wrong-audience rejected; RBAC.

### E — seed → passing e2e
- [x] `e2e/semantic-model/run.py`: upload model + data, POST each golden query,
      assert rows == golden (replaces the `404 pending` probe in `seed.py`).

### F — Great Expectations layer (the tutorial's subject)
- [x] `e2e/great-expectations/`: real `great_expectations` validates the
      executeQueries rows — the tutorial's suites (`row_count_between`,
      `column_values_between`, `values_in_set`, valid-zip) + a checkpoint.
- [x] Assert the pass/fail pattern mirrors the tutorial (Store/Measure pass, the
      YoY ratio `1.8` fails).

### G — DMV / schema rowset boundary
- [x] `$SYSTEM.DISCOVER_STORAGE_TABLES` is explicitly outside the HTTP
      executeQueries scope: it is an XMLA schema rowset consumed through native
      ADOMD.NET, not DAX accepted by this endpoint. The parser's unsupported
      query tests prove it fails loudly; the GX DMV suite remains intentionally
      unavailable until an XMLA transport has a CI-runnable oracle.

### H — CI, coverage, docs
- [x] CI jobs (3-OS, pure-wheel): `e2e/semantic-model/run.py`,
      `e2e/great-expectations/run.py`.
- [x] Go unit tests under the ≥90% coverage gate (total 91.2%).
- [x] Parity doc: Power BI row → 🟢 executeQueries DAX subset with imported and
      Direct Lake data; deferred XMLA/SemPy, full DAX, and DMV. Roadmap entry.
      Swagger `PROVENANCE.md` "Used by".

## Honesty boundaries (documented, never faked)
- executeQueries REST is what *this* plan built; XMLA/SemPy is deferred on
  **cost, not feasibility** (`e2e/xmla` measured ADOMD.NET as Linux-capable and
  endpoint-overridable — see [32-xmla-plan.md](32-xmla-plan.md)).
- DAX **subset**, not full DAX — oracle is captured golden fixtures.
- Imported `data.json` and Direct Lake entity partitions are supported; other
  partition modes and advanced Delta features remain outside this DAX subset.
- DMV/schema-rowset asset deferred to G.

## Progress log
- **A–E done** (2026-07-14): TMSL parse, data binding, DAX evaluator, the
  executeQueries endpoint (handler tests + server e2e), and the passing
  `e2e/semantic-model/run.py` — real PBI token → DAX → golden rows. Total
  coverage 91.2%. Next: F (Great Expectations), then H (CI + parity/roadmap).
- **F + H done** (2026-07-14): real Great Expectations validates the
  executeQueries results — Store/Measure suites pass, the YoY-ratio DAX asset
  fails (1.8 out of band), mirroring the tutorial. CI jobs added (3-OS) for
  both e2es; parity doc Power BI → 🟢 (DAX subset). At that milestone G (DMV)
  and Direct Lake remained deferred; Direct Lake is completed below.
- **Direct Lake done** (2026-07-31): compatibility-level-1604 entity partitions
  resolve the shared OneLake expression, enforce source RBAC, map source columns,
  and query the current Delta snapshot. `e2e/data-science-loop` proves a
  Sail/PySpark-written table flows through DAX, MLflow, and dbt-duckdb. XMLA/DMV
  remains the transport boundary.
