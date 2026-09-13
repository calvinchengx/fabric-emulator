# Unreleased (after v0.36.0)

Draft of what landed on `main` after the `v0.36.0` tag. Rename this file to
`v0.37.0.md` (or whichever minor) when tagging. Open pull requests are not
here.

Two headlines. A **OneLake security fix in the over-granting direction**: a row-
or column-level policy authored the way Microsoft's REST reference documents it
was enforced as no policy at all. And **item permissions**: sharing one item
without the workspace, enforced on OneLake, Direct Lake, `executeQueries` and the
SQL endpoint. Around them, Direct Lake learned to ask OneLake security at all,
and three docs claims that had stopped being true were corrected.

**Read Upgrading before you bump.** Four behaviours tighten, each toward what
Fabric documents: flat-shape OneLake roles now deny, `executeQueries` needs Build,
Direct Lake over an item with no roles needs ReadAll, and a demoted SQL principal
loses its old database roles.

---

## A documented row or column policy was enforced as no policy

`PUT …/dataAccessRoles` carries row and column security inside
`decisionRules[].constraints`, keyed by `tablePath`. The emulator read flat
`rows` / `columns` fields directly on the rule instead — the `principalAccess`
*response* shape, carried over to the *authoring* payload. A rule carrying
`constraints` parsed as unrestricted, and a narrowed Viewer read the whole
table on every enforcement path: DFS and Blob, the Spark two-context split,
`principalAccess`, and Direct Lake.

Nothing caught it because every enforcement witness authored policy in the
emulator's own dialect, and the one Microsoft-client witness (`fab`) round-trips
a role with no constraints.

- The documented shape is read, **per table**: one rule can grant `*` and filter
  only `Tables/sales`. `pkg/onelakesec` gained `DecisionRule.Constraints`.
- The decision rule is decoded **strictly, and fails closed**: an unknown field,
  an unknown permission attribute, an empty column list, a non-`Permit` column
  effect, a non-`Read` column action, or two constraints on one table drops the
  rule. Dropping is always safe — a constraint narrows only its own rule.
- `Narrowing` takes the **most specific** covering entry. Under per-table
  constraints, letting a broader unrestricted entry win erased the filter beside
  it. The Spark agent already read it this way, so the Go and Python halves had
  disagreed on overlapping grants.

Witnessed with the REST reference's own sample payloads, every fail-closed case
beside a readable twin, and the e2e suites (`duckdb`, `two-context`, `livy`)
rewritten to author the documented shape.
[docs/54](../54-onelake-security.md#the-authoring-payload-was-the-wrong-shape-and-every-witness-spoke-it)

## Item permissions: sharing one item without the workspace

Fabric's Core REST has no item-permissions operation — the portal shares through
calls Microsoft does not document — so the emulator builds three surfaces over
one store, and not the portal's internals:

- **Power BI's documented dataset-users API** for semantic models,
  `GET/POST/PUT /v1.0/myorg/datasets/{id}/users` and the in-group spellings, with
  its rules: Post needs ReadReshare, Get and Put ReadWriteReshare, Write can be
  neither added nor removed, an App cannot be a target, `None` removes.
- **An authenticated emulator-native** `…/items/{iid}/_emulator/access` for every
  other item. Under `/v1/workspaces`, not the unauthenticated `/_emulator/`
  prefix, where anyone could grant themselves ReadAll.
- **Fabric's documented admin list** `…/admin/workspaces/{wid}/items/{iid}/users`,
  reporting effective access — inherited rows are an inference, graded as one.

Effective access is the workspace role's implied permissions unioned with a
direct grant, so revoking a grant leaves what the role gives. A grantor shares at
most what they hold. `Write` and `Execute` are refused by name, since nothing
would enforce them. A UPN is refused: there is no directory to resolve it.

Enforced everywhere data is read:

- **OneLake** — a ReadAll grant admits a Viewer, or a principal with no workspace
  role, to that one item on DFS and Blob, listing included. Five surfaces now ask
  one decision, `store.OneLakeReadAccess`, so no two can disagree.
  `fabricItemMembers.sourcePath` is honoured: it was ignored, so a role for
  holders of ReadAll on another item admitted ReadAll holders here.
- **`executeQueries`** needs Read **and Build**, as its reference states.
- **Direct Lake** reads its source through the same OneLake decision.
- **The SQL endpoint** — Read connects, ReadData reads, sharing never writes. A
  revoke takes `CONNECT` away, which also stops a three-part name from another
  granted database and defeats an explicit T-SQL `GRANT` left behind.

The SQL witnesses run through the real relay against a real SQL Server, and were
mutation-checked: with memberships only added, or `CONNECT` left in place, they
fail. [docs/57](../57-item-permissions.md)

## Semantic-model roles apply row-level and object-level security, where they were silently ignored

Neither the TMSL nor the TMDL parser read a model's `roles`, so a model built to
show a Viewer one region evaluated for them over every region. Roles are now
parsed and applied to the principals the product applies them to — those
without Write — on REST `executeQueries` and every XMLA route:

- Members are matched by `memberId`, or by `memberName` against the token's UPN
  (`preferred_username`, else `upn`). Groups match nobody.
- Filters are evaluated per row over a bounded DAX subset; roles are additive; a
  principal in no role gets empty tables; filters travel active relationships
  one → many, transitively.
- Tables and columns a caller's roles hide do not exist for them, in queries or
  in TMSCHEMA rowsets; a hidden key column still joins; measures reading a
  hidden object are hidden. An object is hidden only when every role of the
  caller's hides it.
- Refused by name, for restricted callers: filters outside the subset,
  `bothDirections` and many-to-many relationships, service principals, row and
  object security from different roles, and relaying them to an attached
  msmdsrv. A secured table between two others is refused for everyone. `impersonatedUserName` on a
  secured model is refused for everyone.
- The portal runner has no principal and refuses a secured model.

Write holders are unaffected. Separately, a `SUMMARIZECOLUMNS` group column that
does not exist now errors instead of returning one BLANK group.
[docs/58](../58-semantic-model-roles.md)

## Direct Lake applies OneLake security

A Direct Lake query checked only that the caller held some workspace role, then
read the Delta. OneLake security was never consulted.

- A table no role grants **cannot be resolved**, and a column outside the
  projection is reported missing **by name** — the product's own shape for this
  ("Can't find table", "Column can't be found"), not a 403.
- A **row filter is refused**, not applied. Real Fabric filters; this read is
  pure Go over Delta with no engine to evaluate a predicate. Graded 🟡, and a
  test fails when the refusal becomes a filter.
- Contributor and above are never narrowed. An item with **no** roles requires
  Read and ReadAll, as Fabric does — decided by the same `store.OneLakeReadAccess`
  the storage surface asks, now that item permissions exist to grant ReadAll.

## `dataAccessRoles` refuses item types that cannot carry them

A `PUT` against a Warehouse (or any type outside `Lakehouse`,
`MirroredDatabase`, `MirroredAzureDatabricksCatalog`) returns `400
DataAccessRolesNotSupported`. A warehouse is secured by T-SQL, so a role stored
on one was a policy the emulator honoured and a tenant ignored. The `GET` is not
gated: the docs do not say what it returns for an unsupported item.

## Docs that had stopped being true

- `docs/07` called T-SQL RLS, CLS and masking a non-goal; they shipped in
  `docs/55`. Split into T-SQL (shipped) and semantic-model roles (not modelled —
  and silently dropped today).
- `docs/07`'s OneLake security row still carried a path-read caveat the
  two-context split had removed.
- `docs/03` listed semantic-model evaluation and KQL execution as non-goals,
  directly above the paragraph saying real compute is not one.
- `parity.md` graded Direct Lake inside the `executeQueries` row, one verdict
  over the evaluator, binding, source resolution and authorization. Split into
  its own rows, three of them not green.

## Upgrading

- **OneLake security roles with flat `rows` / `columns` on a decision rule now
  deny that rule's scope.** Rewrite them into `constraints`:

  ```json
  "constraints": {
    "rows":    [{ "tablePath": "/Tables/sales", "value": "SELECT * FROM sales WHERE region = 'us'" }],
    "columns": [{ "tablePath": "/Tables/sales", "columnNames": ["region"],
                  "columnEffect": "Permit", "columnAction": ["Read"] }]
  }
  ```

  Policies already authored in this shape were under-enforced and are now
  enforced.
- **A Viewer querying a Direct Lake model over a lakehouse with OneLake security
  roles** is now narrowed or refused according to those roles.
- **`executeQueries` requires Build.** A workspace Viewer inherits Read only on a
  semantic model, so querying as a bare Viewer now returns `403`. Grant Build
  (`ReadExplore`) through `PUT /v1.0/myorg/datasets/{id}/users`, or query as
  Contributor or above.
- **Direct Lake over an item with no OneLake security roles requires ReadAll.** A
  Viewer is refused until ReadAll is granted on the source item.
- **A model with security roles refuses principals without Write.** A Viewer, or
  a principal granted Read and Build, querying such a model is refused until role
  evaluation lands; Admin, Member and Contributor read as before.
- **SQL endpoint database roles now follow the current rung.** A principal demoted
  below its old role loses the database roles that role gave, at its next connect.
- **`PUT dataAccessRoles` on a Warehouse** now returns `400`.

Consumers pin by digest, so bump `FABRIC_EMULATOR_VERSION` and
`FABRIC_EMULATOR_DIGEST` together.
