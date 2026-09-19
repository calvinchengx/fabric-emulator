# Unreleased (after v0.39.0)

Draft of what landed on `main` after the `v0.39.0` tag. Rename this file to
`v0.40.0.md` (or whichever minor) when tagging. Open pull requests are not
here.

## Ten routes that a real client was already driving now count as covered (#517)

The route-coverage list named four clusters with no traffic, and ten of those
routes were being driven every CI run by suites that did not record — so they
read as uncovered while a real client exercised them. Recording was enabled in
the four candidates and each was scored by its **marginal** contribution:
`e2e/data-science-loop` +6 (the tenant scanner, `admin/workspaces/modified`, the
group-scoped dataset refresh history) and `e2e/eventstream` +4 (reflex triggers,
eventstream destinations, source events) were kept; `e2e/notebook-run` +1 and
`e2e/deployment-pipelines` +0 were reverted, because one route does not justify
coupling the conformance gate to a suite. **The eventstream baseline is the Sail
engine's**, not the default: the JVM engine reads `…/sources/{did}` where Sail
reads `…/sources/{did}/events`, and CI runs Sail.

New traffic found a new disagreement, pinned rather than fixed: five
Reflex-triggered runs report `invokeType: "EventTriggered"`, outside Microsoft's
enum of `Scheduled` and `Manual`. It is the emulator's own distinction between a
job an Activator trigger started and one a person did, five e2e assertions
depend on it, and what real Fabric reports cannot be settled without a tenant.

## Five more routes are driven, and Move Item now answers what Microsoft documents

`e2e/fabric-cli` gains `job start` / `run-status` / `run-cancel` (typed fab
verbs, covering the cancel route and the job-instance read) and a `fab api`
passthrough for `PATCH /items/{id}`, `…/move` and `bulkMove`. **Route coverage
goes 101 → 116 of 172** across both changes.

**`fab mv` is not the client for those routes**, and this was measured rather
than assumed: in the locked fab (1.2.0) an item move is `getDefinition`, a
create and a delete — a copy — and never a `PATCH`. An earlier draft of the
section trusted a newer release's source, produced a comment that was false for
the version that runs, and passed for the wrong reason; the recording is what
exposed it. The driver's own comment claiming the suite pinned fab 1.7.0 was
also stale (the lock pins 1.2.0) and is corrected.

**Move Item answered a bare item where Microsoft documents `{"value": [Item]}`**,
found by conformance the first time a recording reached that route. It now
answers the documented envelope, the one `bulkMove` already used. `fabric-cicd`
calls this route on a redeploy and stores the response without reading a field
from it, so that witness is unaffected.

**A limit a real client found.** `fab acl ls <item>` reads the admin item-users
list and fails inside fab on `principal.displayName`. Microsoft's spec marks that
field optional and fab evidently relies on real Fabric sending it (unconfirmed
without a tenant); this emulator has no directory to
source a name from and omits what it cannot source rather than inventing it. The
response is spec-conformant and unusable by Microsoft's own client, which is
asserted as a failure so it cannot close unnoticed and recorded in the parity row.

## Upgrading

- **`POST /v1/workspaces/{id}/items/{id}/move` answers `{"value": [Item]}`**, not
  the bare item. A consumer reading `id` or `folderId` from the top level of the
  response will not find them; read `value[0]`. Its own status and what the
  store holds are unchanged.
