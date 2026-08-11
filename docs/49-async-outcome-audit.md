# 49 — The asynchronous-outcome audit

Several Fabric APIs document **two** outcomes: a synchronous answer and a
`202 Accepted` carrying an operation to poll. Real Fabric uses both. The
emulator produced only the synchronous one on three surfaces, which is the
worst direction for a divergence to point — **emulator-green, tenant-broken** —
and the failure is silent rather than loud.

This doc records the audit: the rule that makes it repeatable, what was
checked, what was found, and what was deliberately not claimed.

## The rule, which is the durable part

The first pass read Responses tables one API at a time. That works and does not
scale. The pattern that emerged is much cheaper:

> **Every surface that answers 202 says so in its description, in these words:**
> *"This API supports long running operations (LRO)."*
> **Every surface that does not, does not.**

So the sweep is a search, not a reading exercise: find that sentence in the REST
reference, then check the emulator can produce both outcomes for each hit. That
is an instruction someone who has never done this can follow.

It also bounds the problem. The three defects below were **anomalies, not the
front of a queue** — six further surfaces were checked and every one was clean.

## Why this class hides

A client that meets only the synchronous answer never exercises its own polling
path. Then a tenant answers 202 and the failure is quiet:

| API | what the client reads | what it concludes |
|---|---|---|
| `getDefinition` | the 202 body, which is `null` | the item has an **empty definition** |
| `createItem` | `body["id"]` on a 202 | `None["id"]` — a crash three calls downstream |

Neither reports an error. The first is the dangerous one: an empty definition is
a plausible answer, so nothing looks wrong.

Both were found by calling a real tenant, not by reading code — `getDefinition`
here, `createItem` independently while running `examples/medallion-pyspark`
against real Fabric.

## What was found and fixed

| Surface | Documented | Emulator was | Fix |
|---|---|---|---|
| `getDefinition` | 200 / 202 | 200 only | `FABRIC_FORCE_LRO` |
| `createItem` | 201 / 202 | 201 unless a definition was supplied | `FABRIC_FORCE_LRO` |
| `git/initializeConnection` | 200 / 202 | 200 only | `FABRIC_FORCE_LRO` |

`createItem` was the sharpest: **Create Warehouse** documents 201 and 202, a
tenant answered 202, and the same page says it *"does not support create a
warehouse with definition"* — while the emulator went async **only** for a
definition-bearing create. So the one item type measured asynchronous was the
one type guaranteed synchronous here.

`git/initializeConnection` is the only one of the three whose operation carries
a real **result body**, so it also covers the case where the async answer must
hand back the same object the synchronous one did.

The lever is **off by default**: the synchronous answers are equally legal and
are what most calls see. The point is that the other half is reachable at all —
the same idea as `FABRIC_LIST_PAGE_SIZE=2` forcing the continuation-token loop.

## Audited and CLEAN

Recorded so nobody re-checks them. A negative result is worth as much as a
positive one when the value of a sweep depends on knowing its coverage.

| Surface | Documented | Emulator | Verdict |
|---|---|---|---|
| [`git/connect`][connect] | 200 only | 200 | clean |
| [`git/disconnect`][disconnect] | 200 only | 200 | clean |
| [`admin/domains/{id}/assignWorkspaces`][domains] | 200 only, even in bulk | 200 | clean |
| `updateDefinition` | LRO | 202 already | clean |
| `commitToGit`, `updateFromGit` | LRO | 202 already | clean |
| capacity assign / unassign | LRO | 202 already | clean |
| workspace identity provision / deprovision | LRO | 202 already | clean |
| deployment pipeline deploy | LRO | 202 already | clean |
| item job instances | LRO | 202 already | clean |

Mirroring is a special case: **Update Mirrored Database Definition** supports
LRO, but it routes to the generic `updateDefinition`, which is already
asynchronous — so it is covered without being a separate fix.

## Gaps, which are a different thing from divergences

Documented LROs the emulator does **not implement at all**. These are not wrong
shapes, and a sweep for wrong shapes will not find them:

* **Load Table** (`…/lakehouses/{id}/tables/{name}/load`)
* **`sqlEndpoints/{id}/refreshMetadata`**
* Mirroring **start / stop / status** (`startMirroring`, `stopMirroring`,
  `getMirroringStatus`) — the emulator has its own `refreshMirror` instead

## Not claimed

This is **not** a proof that no fourth divergence exists. It is a record of what
was checked, by a rule that anyone can re-run. Applying the rule to the whole
reference — rather than to the surfaces this emulator already implements — is
the obvious next pass, and it has not been done.

[connect]: https://learn.microsoft.com/en-us/rest/api/fabric/core/git/connect
[disconnect]: https://learn.microsoft.com/en-us/rest/api/fabric/core/git/disconnect
[domains]: https://learn.microsoft.com/en-us/rest/api/fabric/admin/domains/assign-domain-workspaces-by-ids
