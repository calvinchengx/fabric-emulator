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
