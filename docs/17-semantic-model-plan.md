# 17 — Semantic-model / DAX engine plan

Turn Power BI from 🔴 (item management only) into a real query engine: the
`executeQueries` REST contract backed by a bounded, real DAX evaluator, so the
SemPy + Great Expectations tutorial's *methodology* runs against the emulator.

Grounded in the golden references pinned first:
[16-semantic-model-references.md](16-semantic-model-references.md),
[third_party/powerbi-rest-swagger](../third_party/powerbi-rest-swagger/) (the
executeQueries OpenAPI), and [e2e/semantic-model](../e2e/semantic-model/) (the
golden model + hand-computed DAX oracle).

**Why executeQueries, not XMLA:** SemPy speaks XMLA via native ADOMD.NET, which
can't be pointed at a Go server and has no CI-runnable oracle. executeQueries is
HTTP+JSON with a vendored OpenAPI *and* a live oracle. XMLA/SemPy stays
deferred-with-cause. See doc 16.

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
- [ ] *Deferred, with cause:* **Direct Lake** (tables backed by real OneLake
      Delta) — needs a pure-Go Parquet + `_delta_log` reader; import-seeded first.

### C — the DAX evaluator (core)
- [x] Tokenizer + parser for the subset: `EVALUATE`, `SUMMARIZECOLUMNS`, table /
      column / measure refs, function calls, string literals.
- [x] Evaluation: filter context, relationship traversal (`Sales`→`Time`/`Store`),
      measure expansion, `SUM`, `DIVIDE` (blank on ÷0), `EVALUATE <table>`,
      `SUMMARIZECOLUMNS(cols…, "name", expr)`.
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

### G — DMV / schema rowset (deferred sub-phase)
- [ ] `$SYSTEM.DISCOVER_STORAGE_TABLES` → `RIVIOLATION_COUNT` (0 for the clean
      model) so the DMV asset works; until then the GX DMV suite is pending.

### H — CI, coverage, docs
- [x] CI jobs (3-OS, pure-wheel): `e2e/semantic-model/run.py`,
      `e2e/great-expectations/run.py`.
- [x] Go unit tests under the ≥90% coverage gate (total 91.2%).
- [x] Parity doc: Power BI row → 🟢 executeQueries DAX-subset; deferred XMLA/SemPy,
      full DAX, DMV, Direct Lake. Roadmap entry. Swagger `PROVENANCE.md` "Used by".

## Honesty boundaries (documented, never faked)
- executeQueries REST, **not** XMLA/SemPy (native ADOMD.NET, no CI oracle).
- DAX **subset**, not full DAX — oracle is captured golden fixtures.
- **Import-seeded** data, not Direct Lake (first cut).
- DMV/schema-rowset asset deferred to G.

## Progress log
- **A–E done** (2026-07-14): TMSL parse, data binding, DAX evaluator, the
  executeQueries endpoint (handler tests + server e2e), and the passing
  `e2e/semantic-model/run.py` — real PBI token → DAX → golden rows. Total
  coverage 91.2%. Next: F (Great Expectations), then H (CI + parity/roadmap).
- **F + H done** (2026-07-14): real Great Expectations validates the
  executeQueries results — Store/Measure suites pass, the YoY-ratio DAX asset
  fails (1.8 out of band), mirroring the tutorial. CI jobs added (3-OS) for
  both e2es; parity doc Power BI → 🟢 (DAX subset). Only G (DMV) + Direct Lake
  remain deferred-with-cause. The plan is complete.
