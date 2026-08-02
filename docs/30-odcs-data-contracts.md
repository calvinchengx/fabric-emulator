# 30 — ODCS: data contracts over the medallion

**Status: mapped, contracts written and schema-valid, not yet enforced.** Three
[ODCS v3.1.0](https://bitol-io.github.io/open-data-contract-standard/) contracts
live in [`examples/medallion-pyspark/contracts/`](../examples/medallion-pyspark/contracts/) and
validate against the standard's own JSON Schema. Nothing checks them against
reality yet — that is O1–O4 below, and until it exists these files are
documentation, not a guarantee.

[28-tutorial-end-to-end.md](28-tutorial-end-to-end.md) is the pipeline this maps
over; [22-openmetadata.md](22-openmetadata.md) is the catalog it sits beside.

## The thesis: declared versus observed

The family already has a governance story, and it is deliberately one-directional.
`govern-ingest` reads the **real `_delta_log`** and publishes what it finds; the
rule is that it "emits lineage only where the emulator holds an **exact** fact —
it never infers a graph." OpenMetadata answers *what is actually there*.

ODCS answers the opposite question: *what did we promise would be there*. It is
a declaration, written ahead of the data, owned by people rather than derived
from bytes.

Neither is worth much alone. A catalog with no contract tells you the shape of
whatever happened to land. A contract with no catalog tells you what someone
intended in a document nobody re-reads. **The value is in the subtraction** —
declared minus observed is drift, and drift is the thing that actually breaks
consumers:

| | ODCS | OpenMetadata (`govern-ingest`) |
|---|---|---|
| Answers | what we promise | what is there |
| Source | authored by the data owner | read from `_delta_log` / TDS |
| Written | before the data | after the data |
| Fails when | reality diverges from the promise | never — it just reports |
| Lives in | `examples/medallion-pyspark/contracts/*.odcs.yaml` | the catalog service |

That subtraction is the whole reason to bother, and it is what O2 builds.

## Where contracts belong — and where they do not

The instinct is one contract per medallion layer, four contracts, done. That is
wrong, and the wrongness is instructive: **a data contract is an agreement
between a provider and a consumer.** Where there is no consumer, there is no
agreement to write, and a file that pretends otherwise is worse than no file.

| Layer | Consumer | Contract? | Why |
|---|---|---|---|
| **Landing** | bronze only, but the **provider is external** | ✅ `landing-contoso-pos.odcs.yaml` | The agreement is with the vendor. This is the one contract the pipeline team does not author — they receive it. |
| **Bronze** | nothing outside the pipeline | ❌ **deliberately none** | Bronze keeps everything: duplicates, the malformed row, five spellings of three countries. Every guarantee worth stating is *false* here. See below. |
| **Silver** | gold | ✅ `silver-sales.odcs.yaml` | Internal, same team — but the guarantees are real and currently invisible, living as Python asserts. |
| **Gold** | the semantic model, Power BI, anything downstream | ✅ `gold-sales.odcs.yaml` | The published product. If only one contract is ever written, it is this one. |
| **Semantic model** | report authors | ❌ not ODCS | Out of scope — see "What ODCS does not cover". |

### Why bronze gets no contract

Bronze is defined by [`bronze.py`](../examples/medallion-pyspark/bronze.py) as
"land the raw export into Delta tables, keeping everything — duplicates and the
malformed row included." Any `unique`, `not_null` or `accepted_values` rule on
bronze would be a promise the layer exists to *not* make. What is left after
removing them is a column list — which the catalog already reports, more
accurately, from the Delta log.

The temptation is to write one anyway "for completeness". Resist it: a contract
whose rules are all trivially true trains people to stop reading contracts.

The one genuinely useful thing to say about bronze is that it is dirty, and that
belongs in the *landing* contract's `description.limitations`, where a consumer
deciding whether to read raw data will actually see it.

## Section-by-section mapping

Every ODCS section has a source of truth that already exists in this repo. The
mapping is what makes the contracts checkable rather than decorative:

| ODCS section | Source of truth here | Notes |
|---|---|---|
| `apiVersion`, `kind`, `id`, `version`, `status` | the file | `id` is a UUIDv5 over the contract name, so it is stable and re-derivable rather than a random value nobody can regenerate |
| `domain`, `dataProduct`, `tenant` | workspace / item naming | `tenant: fabric-emulator` marks these as emulator-scoped |
| `servers` | the emulator's endpoints | `type: azure` + `format: delta` for OneLake; `type: sqlserver` for the TDS surface. `database` is an item **GUID created at run time**, so it is a `${...}` placeholder — a literal would be a lie one run later |
| `schema` objects/properties | the **real Delta log** (delta-rs) and `INFORMATION_SCHEMA` | the same two sources [`govern_ingest.py`](../scripts/govern_ingest.py) already reads |
| `quality` | dbt tests in [`gold/models/schema.yml`](../examples/medallion-pyspark/gold/models/schema.yml) and the asserts in [`silver.py`](../examples/medallion-pyspark/silver.py) | mapped rule-by-rule below |
| `slaProperties` | landing partition freshness | the example runs daily |
| `support`, `team`, `roles` | entra principals and workspace roles | only `support` is populated; roles would duplicate what the control plane holds |
| `authoritativeDefinitions` | docs and code URLs | for a real vendor this is where the **OpenAPI document** goes |

### `physicalType` is deliberately missing on properties

None of the three contracts declares a physical column type. Nobody has measured
what the warehouse reflects `amount` or `order_date` as — the path runs pandas →
delta-rs → Delta → the SQL analytics endpoint → `SELECT … INTO`, and every hop
gets a say.

Writing a plausible `DECIMAL(18,2)` would be exactly the silent approximation
this repo refuses elsewhere. The fields are absent, O1 measures them, and the
values that land are then facts rather than guesses.

## The quality mapping, rule by rule

This is where the mapping stops being administrative. Gold's dbt tests translate
cleanly until they don't:

| dbt test | ODCS rule | Type |
|---|---|---|
| `not_null` | `metric: nullValues`, `mustBe: 0`, `dimension: completeness` | library |
| `unique` | `metric: duplicateValues`, `mustBe: 0`, `dimension: uniqueness` | library |
| `accepted_values` | `metric: invalidValues`, `mustBe: 0`, `arguments.validValues: [...]`, `dimension: conformity` | library |
| `relationships` | **no library metric exists** — hand-written SQL | ⚠️ sql |
| `assert_no_negative_revenue` (singular) | `type: sql` with `{object}` substitution | sql |

**ODCS's library has exactly five metrics** — `nullValues`, `missingValues`,
`invalidValues`, `duplicateValues`, `rowCount` — and referential integrity is
not among them. So the single most common cross-table guarantee in a star
schema, "every fact row resolves to a dimension row", drops straight out of the
portable vocabulary into a `type: sql` rule carrying dialect-specific text.

That is a real limitation of the standard, not a modelling mistake, and it has a
consequence worth stating: **the sql rules are the ones that will rot.** A
library rule is engine-independent; a sql rule embeds T-SQL, so it must run
against the warehouse or not at all. Keep the count of them small and know which
they are.

`{object}` and `{property}` placeholders are substituted by the runner, which is
the only thing keeping those queries from also hard-coding table names.

## The drift problem this introduces

Adding these files creates a **third** copy of the same rules:

1. `gold/models/schema.yml` — dbt tests, executed by `dbt build`
2. `silver.py` asserts + `EXPECTED_*` in `source_system.py` — executed by the pipeline
3. `contracts/*.odcs.yaml` — executed by nothing

Three copies of a rule is worse than one copy, because two of them will quietly
stop agreeing and nobody finds out until the disagreement matters. The repo
already has a precedent for the fix: the portal's `dist/` is committed **and**
CI fails on drift. Same shape applies here.

**Decision: both stay authored, and CI fails on divergence.** The contract and
the dbt tests are each written by hand, in the form natural to their own tool,
and a check asserts they agree. This is what O3 builds.

The two alternatives, and why not:

- **ODCS as source of truth, generating dbt's `schema.yml`.** Drift becomes
  structurally impossible, which is strictly stronger. Rejected for now because
  it costs a generator and makes the dbt project partly non-hand-editable — an
  odd thing to inflict on an *example* whose job is to be read and copied. Worth
  revisiting if the rule count grows.
- **dbt as source of truth, deriving the contract.** Cheapest, but it inverts
  the point: a contract is supposed to be authored ahead of the data by its
  owner. Derived from the implementation, it can only ever restate what was
  built, never constrain it.

The chosen option is the weakest of the three at *preventing* drift and the only
one that keeps both artifacts idiomatic. That trade is deliberate: the failure
mode it leaves open — someone edits one file and not the other — is caught by CI
within a single push, which is soon enough.

The silver asserts are a separate case: they are *fixture* assertions
(`rowCount mustBe 6`) rather than production rules. The contract says so
in-line, because a contract that pins an exact row count is only defensible when
the input is a fixture.

## Making it real — the plan

The current state is the honest one: files that a validator accepts and nothing
else touches. Four milestones turn them into a guarantee. Each is small and each
produces a witness, in the pattern of
[29-tsql-parity.md](29-tsql-parity.md).

| # | Milestone | What lands | Witness |
|---|---|---|---|
| **O1** | **Measure the physical types.** Run the medallion stack, read the Delta log and `INFORMATION_SCHEMA`, fill in every `physicalType`. | corrected contracts | values are measured, not authored |
| **O2** | **The drift check.** `scripts/check_contracts.py`: for each contract, resolve its servers, read the observed schema, and fail on any column that is declared-but-absent, absent-but-declared, or type-mismatched. | the declared-minus-observed subtraction | `ci:medallion` |
| **O3** | **Rules are enforced, not decorative.** Assert every ODCS `quality` rule maps to something that actually runs — a dbt test or a pipeline assert. A rule with no enforcer fails the check. | the third-copy problem closed | `ci:medallion` |
| **O4** | **Execute the contract directly.** Run the library and sql rules against the warehouse over TDS, so the contract can fail data on its own rather than by proxy. | contract-as-test | `ci:medallion` |

O2 is the one that carries the value; O1 is its prerequisite. O3 is cheap and
prevents the rot the drift section describes. O4 is optional — it overlaps dbt,
and is worth doing mainly to prove the emulator's TDS surface is enough for a
contract runner (`datacontract-cli` speaks ODCS v3 and has a `sqlserver` server
type, so the interesting question is whether it drives *this* endpoint).

**None of these are started.** No parity claim has been added to
[parity.md](parity.md), because there is nothing yet to witness.

## Binding a real source system

The landing contract is the one that would change most in a real engagement,
because its provider is external. Two things make that binding concrete:

- **`authoritativeDefinitions` points at the vendor's OpenAPI document**, not at
  a description of it. The stub currently points at
  [`source_system.py`](../examples/contoso-fixtures/source_system.py) and says so.
- **The schema is generated from that spec, not typed by hand.** An OpenAPI
  `components.schemas` entry maps to an ODCS object almost one-for-one:
  `properties` → `properties`, `required` → `required: true`, `enum` →
  `arguments.validValues`, `format: date-time` → `logicalType: timestamp`.

Realistic sources worth modelling, if this example ever grows past one vendor:
a POS/commerce API (Shopify, Square) for order events; a CRM (Salesforce,
HubSpot) for the customer master; a billing system (Stripe) for revenue
recognition. The useful property is not realism for its own sake — it is that
each *overlaps* on customer identity, which is what forces conformance to be a
real step in silver rather than a rename.

Two sources is the smallest number that makes the medallion honest, because
conforming one source to itself is not conforming. The current example has one,
which is why silver's interesting work is deduplication rather than resolution.

## What ODCS does not cover

Stated plainly so nobody goes looking:

- **Semantic models.** ODCS describes datasets. A semantic model is a *product*
  built over one — measures, hierarchies, DAX. Its sibling standard ODPS (Open
  Data Product Standard) is the right shape; [18](18-semantic-model-references.md)
  is where the emulator's boundary already lives.
- **Lineage.** No section for it. The catalog holds lineage, and holds it as
  observed fact — see [22](22-openmetadata.md).
- **Transformations.** `transformLogic` exists at property level, but it is a
  free-text note, not an executable definition. dbt remains the source of truth
  for how a column is computed.
- **Referential integrity**, as covered above.
- **Access control.** `roles` names roles; it does not grant them. Entra and the
  workspace RBAC do.

## Files

- [`contracts/landing-contoso-pos.odcs.yaml`](../examples/medallion-pyspark/contracts/landing-contoso-pos.odcs.yaml) — inbound, from the vendor
- [`contracts/silver-sales.odcs.yaml`](../examples/medallion-pyspark/contracts/silver-sales.odcs.yaml) — internal
- [`contracts/gold-sales.odcs.yaml`](../examples/medallion-pyspark/contracts/gold-sales.odcs.yaml) — published

All three validate against
[`odcs-json-schema-v3.1.0.json`](https://github.com/bitol-io/open-data-contract-standard/blob/main/schema/odcs-json-schema-v3.1.0.json).
