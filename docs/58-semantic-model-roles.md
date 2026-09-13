# 58 — Semantic-model roles: row-level and object-level security

**Status: all four stages built — row-level and object-level security are
applied to the principals the product applies them to.**

**Decision: apply a model's roles in the loader every row-returning path shares,
for the principals the product applies them to, and refuse by name anything the
bounded evaluator cannot apply.**

Grounded against Microsoft Learn as read on 2026-09-13/14: *Row-level security
(RLS) with Power BI*, *Object-level security (OLS) with Power BI*, *Analysis
Services tabular model object-level security*, *Roles object (TMSL)*, *TMDL
overview*, and the *Datasets — Execute Queries* REST reference.

## What Fabric does

| Rule | Source |
|---|---|
| RLS and OLS apply **only to principals without Write**: Viewers, and principals granted Read or Build. Admin, Member and Contributor are exempt, and Build does not exempt | RLS; OLS |
| A `filterExpression` is DAX evaluated **TRUE/FALSE per row**; only TRUE rows survive | RLS |
| Roles are **additive**; a principal in no role on a secured model sees no data | RLS |
| Security filters flow along relationships **single-direction by default** | RLS |
| `USERPRINCIPALNAME()` and `USERNAME()` both return the **UPN** in the service | RLS |
| Members are users by UPN, or groups; **service principals cannot be members**, and `executeQueries` does not support them on RLS models | RLS; Execute Queries |
| OLS `metadataPermission: none` makes a table or column behave **as if it does not exist** | OLS |
| RLS and OLS **from different roles** is a query-time error; a secured table cannot break a relationship chain; measures referencing a secured object are secured | AS OLS |

The TMSL shape is the Roles object: `name`, `modelPermission`
(`none|read|readRefresh|refresh|administrator`), `members[]` (`memberName`,
`memberId`, `identityProvider`, `memberType`), and `tablePermissions[]` (`name`,
`filterExpression` as a string or lines, `metadataPermission`,
`columnPermissions[]`). TMDL writes `role`, `tablePermission Table = <DAX>`,
`columnPermission Column = none` and `member Name = user`, one file per role.

## What the emulator did

- **Roles were dropped, silently**, by both parsers. A model built to show a
  Viewer one territory evaluated for them over every territory, and nothing
  failed. Definitions were stored verbatim, so round trips kept the roles the
  evaluator ignored.
- **The DAX evaluator cannot evaluate a filter.** It has no row context, no `||`,
  `&&` or `IN {}`, and no `TRUE()`, `FALSE()` or `USERPRINCIPALNAME()`.
- **Principals carry no UPN**, although entra-emulator mints `preferred_username`.
- **Relationships carry no direction**, and propagation is undirected and
  single-hop.
- **XMLA ran every MWC-token call as whoever exchanged last**, so RLS over XMLA
  could not have known the caller. Fixed first, on its own
  ([docs/32](32-xmla-plan.md#correction-2026-09-14-the-mwc-token-became-a-credential-in-production-not-just-in-the-screens)).

## Staging, and what each stage may claim

| Stage | Build | May claim |
|---|---|---|
| 1 ✅ | Roles parsed from TMSL and TMDL. A principal without Write on a model with roles is refused on every row-returning path: REST `executeQueries`, the XMLA loader, the portal runner | **Never serves unfiltered rows to a principal a role restricts** |
| 2 ✅ | `Principal.UPN` from `preferred_username`/`upn`; membership by `memberId` or `memberName`↔UPN; groups match nobody; service principals below Write refused, as documented | Membership resolves the way the service resolves it |
| 3 ✅ | A bounded DAX row-predicate evaluator; filters unioned per table across a principal's roles; no role, no rows; propagation one-to-many, transitively; applied in the shared loader for REST and XMLA | Filtered rows on REST and XMLA |
| **4 ✅** | Tables and columns pruned for restricted principals, hidden keys still joinable; dependent measures hidden; `SUMMARIZECOLUMNS` errors on a missing column instead of returning a BLANK group; RLS+OLS across roles and chain-breaking tables error | Secured objects do not exist in DAX or the TMSCHEMA rowsets |

### Stage 1 in detail

*Superseded by stage 3, which applies the filters in the same place; kept as the
record of why the refusal came first.*

The refusal lives in `loadSemanticModel`, the loader REST `executeQueries` and
all three XMLA routes share, so a Viewer cannot reach the rows by changing
protocol. The portal runner has no principal, so it refuses any model with roles.
The msmdsrv relay is reached only after that loader, and its catalog is deployed
without roles, which only ever serves Write holders — whom the product exempts.

A model with **any** role refuses, whatever the role contains: "users who aren't
assigned to any RLS role typically see no data", so a secured model has no
unfiltered answer for a restricted principal at all.

### Stage 3 in detail

`loadSemanticModel` decides once whether the roles restrict the caller (no Write
on the item) and, if so, resolves the caller's roles and applies them after the
import and Direct Lake data are loaded — so REST `executeQueries` and all three
XMLA routes return the same filtered rows. `ApplyRowSecurity`:

- **No role, no rows**: every table comes back empty, as the product describes
  for a principal in no role, rather than the query being refused.
- **Additive**: a table is narrowed only if every admitting role filters it, and
  a row survives if any role's filter admits it.
- **One → many, transitively**: a narrowed table on a relationship's "one" side
  narrows the "many" side to rows whose key survives, repeated to a fixpoint, so
  a filter on a region reaches stores and then sales. Inactive relationships
  carry nothing. A fact row whose key matches no visible dimension row is
  withheld once that dimension is narrowed.

Refused, by name, for the restricted caller only — a Write holder reading the
same model is unaffected:

- an active relationship with `securityFilteringBehavior: bothDirections`, and a
  many-to-many relationship, whose filtering this evaluator does not model;
- a **service principal**, which "can't be added to an RLS role";
- a filter outside the subset, or one DAX would reject at evaluation (text
  compared with a number) — an error, never a quiet FALSE;
- `impersonatedUserName` on any secured model, whoever asks;
- relaying a restricted caller to an attached msmdsrv (`FABRIC_DAX_URL`): its
  catalog is deployed per item without roles, so filtered rows would overwrite
  another caller's catalog. The engine is never contacted for them.

### Stage 4 in detail

After the row filters, `ApplyObjectSecurity` returns a **copy** of the model and
data with what the caller's roles hide removed, so the evaluator and every
TMSCHEMA rowset — both read that copy — answer as if it never existed:

- A hidden table leaves the model, the data, its relationships and its
  measures. Referencing it is the evaluator's ordinary "no table" error.
- A hidden column leaves the model. It stays in the rows only when a remaining
  relationship joins on it: "relationships that reference a secured column work
  provided the table the column is in is not secured".
- A measure that names a hidden table or column, or a hidden measure, is hidden
  ("dynamic calculations … are automatically restricted"). The scan reads
  tokens, so it over-hides rather than under-hides: a bare `[Name]` matching any
  hidden column counts, and an expression that does not lex is hidden.
- **Across roles, an object is hidden only if every one of the caller's roles
  hides it.** *Inferred*: the product documents no combination rule for OLS
  alone. `metadataPermission` defaults to read, so a role that does not mention
  an object grants it, and roles are additive.
- Row filters and hidden objects **from different roles** of the caller are the
  product's query-time error. From one role they combine.
- Securing a table that sits **between two others** — the "many" side of one
  relationship and the "one" side of another — is refused for every caller, Write
  holders included: the product rejects it at design time, so such a model could
  not exist in the service.

Two evaluator gaps closed with it, since OLS depends on a missing name failing:
a `SUMMARIZECOLUMNS` group column that does not exist returned one BLANK group,
and a bare column reference that does not exist was reported as having "no
single value". Both are now the missing-column error.

## The DAX filter subset (stage 3)

Column references (`'T'[C]`, `T[C]`, and `[C]` against the permission's own table
in row context); string and number literals, `TRUE()`, `FALSE()`, `BLANK()`;
`= <> < <= > >=`, `&&`, `||`, `NOT`, `AND`, `OR`, `IN { … }`;
`USERPRINCIPALNAME()` and `USERNAME()`. String comparison is case-insensitive, as
DAX's is. Anything outside it — `LOOKUPVALUE`, `RELATED`, `CUSTOMDATA` — is
refused by name, on the DAX engine's own terms: answer the pinned subset, error
outside it.

## Boundaries

- **`impersonatedUserName`** on a secured model is refused: who may
  impersonate is not sourced, and ignoring it would return the caller's own view.
- **`securityFilteringBehavior: bothDirections`** is refused rather than
  ignored, since ignoring it serves rows it would have removed.
- **Groups** are stored but match nobody, as with item permissions: group
  membership is not modelled.
- **Power BI Desktop's engine** (`e2e/pbix-desktop`) is the available oracle for
  filter semantics with an effective user; it is Windows-only and optional.

## Witnesses

Stage 1: `internal/semanticmodel/roles_test.go` parses Microsoft's own samples
from the TMSL reference, the OLS page and the TMDL overview. Its API refusal
tests became stage 3's filtering tests.

Stage 2: `internal/auth/auth_test.go` reads the UPN for users only;
`internal/semanticmodel/membership_test.go` resolves members by id and UPN,
case-insensitively, and matches no group and no empty identity.

Stage 3: `internal/semanticmodel/rowfilter_test.go` pairs every admission with
the row it must refuse, and names every compile and evaluation refusal;
`rowsecurity_test.go` covers no role, one-way and transitive propagation,
additive roles, inactive relationships and the refusals. Both files cover
`rowfilter.go` and `rowsecurity.go` fully. `internal/api/semanticroles_test.go`
serves a member by id and a member by UPN the West stores and their sales, a
principal with Read and Build but no role nothing, and an Admin and a
Contributor every row, in the same run; covers the XMLA loader, a TMDL model,
additive roles, each refusal, impersonation and the relay. With the filter
skipped, six fail; with the exemption applied to everyone, six fail; each
refusal disabled on its own fails its witness.

Stage 4: `internal/semanticmodel/objectsecurity_test.go` covers a hidden table
(model, data, relationships, measures, queries), a hidden column, a hidden key
that still joins, the combination across roles, `read` hiding nothing, the
mixed-roles error, the chain rule and the missing-column fixes, and covers
`objectsecurity.go` fully. `internal/api/semanticroles_test.go` shows a Viewer
the Store rows without PostalCode, refuses grouping by it and omits it from
`TMSCHEMA_COLUMNS`, while an Admin reads it in the same run; hides a table while
the same role's filter still narrows the facts; shows OLS in a role the caller
is not in leaving them alone and the mixed-roles error for one who is in both;
and refuses a chain-breaking model to an Admin. Each of eight mutations — OLS
not applied, the chain check skipped, any role hiding instead of every role,
measures kept, hidden columns kept in rows, a hidden key stripped, mixed roles
allowed, group columns unchecked — fails at least one witness.
