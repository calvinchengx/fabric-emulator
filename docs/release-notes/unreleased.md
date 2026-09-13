# Unreleased (after v0.36.0)

Draft of what landed on `main` after the `v0.36.0` tag. Rename this file to
`v0.37.0.md` (or whichever minor) when tagging. Open pull requests are not
here.

The headline is a **OneLake security fix in the over-granting direction**: a
row- or column-level policy authored the way Microsoft's REST reference
documents it was enforced as no policy at all. Around it, Direct Lake learned to
ask OneLake security at all, and three docs claims that had stopped being true
were corrected.

**Upgrade if you author OneLake security roles with row or column
constraints.** Read the first section before you do: a role written in the
emulator's old flat shape now denies instead of narrowing.

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

## Direct Lake applies OneLake security

A Direct Lake query checked only that the caller held some workspace role, then
read the Delta. OneLake security was never consulted.

- A table no role grants **cannot be resolved**, and a column outside the
  projection is reported missing **by name** — the product's own shape for this
  ("Can't find table", "Column can't be found"), not a 403.
- A **row filter is refused**, not applied. Real Fabric filters; this read is
  pure Go over Delta with no engine to evaluate a predicate. Graded 🟡, and a
  test fails when the refusal becomes a filter.
- Contributor and above are never narrowed. An item with **no** roles keeps the
  workspace-role gate, because Fabric would require Read and ReadAll there and
  item permissions are not modelled.

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
- **`PUT dataAccessRoles` on a Warehouse** now returns `400`.

Consumers pin by digest, so bump `FABRIC_EMULATOR_VERSION` and
`FABRIC_EMULATOR_DIGEST` together.
