# Unreleased (after v0.38.0)

Draft of what landed on `main` after the `v0.38.0` tag. Rename this file to
`v0.39.0.md` (or whichever minor) when tagging. Open pull requests are not
here.

## Docs that name a Go package or symbol are held to the tree

`check_doc_drift.py` already held three classes of reference to the tree — repo
paths, make targets, env var names — and a backticked `internal/pkg.Name` has
the same shape: a fact that was true when it was typed, which a rename retires
silently. Symbols are indexed from source text rather than `go/types`, and only
top-level declarations in tracked `.go` files count; a generic `func f[T any](`
is why this is not the obvious regex. Methods are deliberately out of the index,
since a doc naming `internal/server.ServeHTTP` is not naming a package symbol.
Zero findings and zero false positives across 92 docs, and the class fires on an
injected dead symbol and an injected dead package. [docs/10](../10-testing.md)

## The portal's four lost styling intents are restored

Four of the 29 dead CSS rules deleted earlier were dead for a shared reason
rather than by obsolescence: the class is handed to a child component, so the
element it would style carries the child's scope hash and never the parent's.
Svelte compiled those rules away and the intent had never applied, so deleting
them cleared the lint without restoring anything. They are re-expressed as
utility classes passed through the child's own `class` prop, which is how these
components style their shadcn children and needs no `:global()`. A failed event
row in the flow log is red again, the DAX textarea is monospace and resizes
vertically only, the query result table has its top margin, and the sidebar
toggle has its optical offset, glyph sizing and muted resting colour. Verified
in the built sheet rather than assumed: every class resolves to a real rule in
`portal/dist/assets`.

## The my-workspace dataset routes had never been called by anything (#509)

Power BI documents two spellings of the same surface — a group-scoped
`/myorg/groups/{g}/datasets/{d}/…` and a group-less `/myorg/datasets/{d}/…`.
Every suite used the group form, so six registered routes had no traffic at
all, `executeQueries` among them: the DAX surface this repository vendored the
Power BI swagger for. What is asserted is that the two forms **agree** — the
emulator resolves the id first and treats the group as a constraint on it, so
the two paths meeting in one handler is an implementation fact rather than a
guarantee. `e2e/semantic-model` hosts it and now records, the first addition to
the recording set since the measurement that declined the other 28; it earns it
on seven routes no other recording reaches.

Two disagreements with the swagger are pinned rather than fixed, because in
both the documented answer would be a well-formed lie: the my-workspace list
answers 404 where the spec documents 200 (there is no personal workspace, and
`{"value": []}` is indistinguishable from an empty one), and a refresh of an
inline-data model answers 400 where the spec documents 202.

## Three gates that could not see what they were guarding (#510)

**The route ratchet could not see 34 of its own routes.** It matched
`HandleFunc("METHOD /path"`, a pattern requiring a closing quote after the
path, so any route built from a variable — `"POST "+prefix+"/refreshes"` —
sat outside the denominator, where nothing can ever be reported as uncovered.
Registered goes 133 → **167** and coverage 84/133 (63%) → **86/167 (51%)**:
the number got worse because it got true. The real fix is that a registration
this parser cannot resolve now either carries a stated reason or fails the run.

**A ledger for the operations no gate could describe.** Route coverage and
conformance both measure against what this emulator *registers*, so neither can
say anything about what Microsoft documents and this emulator does not
implement. `scripts/check_surface_ledger.py` puts all **1002** documented
operations in one of three states — **120 served, 17 refused, 865 silent** —
where a refusal is credited only when a recording shows a suite really asked.
Power BI's nine Imports operations were the worked example: all silent, now all
asserted and refused. Asking found a defect. An unrouted API path answered Go's
plaintext `404 page not found` for every write verb, because the catch-all
checked the method before it checked the path — so the guard written for
`/v1` had only ever covered reads.

**Direct Lake is proved to see data written after publish.** `Completed` proves
nothing on its own and neither does an unchanged answer; both are satisfied by
a model that snapshotted its rows when it was published. `e2e/data-science-loop`
now appends a row mid-suite and both readers, Direct Lake over XMLA and
dbt-duckdb over the Delta log, have to follow it to the same answer.

**sempy reads the refresh surface**, which only `urllib` had ever looked at.
`refresh_dataset` raises `FabricHTTPException` on the emulator's 400, so a real
client is shown to act on a refusal. There is no sempy API for dataset users,
so that surface keeps its raw-HTTP witness only.
