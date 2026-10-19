# 66 — The gate inventory: seventeen enforcing checks no prose page named

**Status: of the 68 scripts under `scripts/`, 30 are named by no prose page in
this repository outside the historical release notes. 17 of those 30 are wired,
enforcing gates — 14 run from `make check`, one from `make portal-types`, one
from `make sbom`, and `scripts/check_advisories.py` from
`.github/workflows/security.yml`. For each of the 17, its denominator, its
ratchet direction and the command that updates it were reviewable only by
reading its source. This document is the missing half.**

The other 13 are build, vendor and developer tooling, counted and named below
rather than dropped, because a chapter that lists only what it covers reads as
complete.

This is the eighth document on this architecture, after
[60-test-flakiness.md](60-test-flakiness.md),
[62-performance-regressions.md](62-performance-regressions.md),
[63-security-footguns.md](63-security-footguns.md),
[64-logging-quality.md](64-logging-quality.md) and
[65-api-surface-coverage.md](65-api-surface-coverage.md). It follows 65's
precedent in the way that matters: **it lands no new checker and changes no
gate.** Every gate below already exists and already works. What was missing is
the prose.

## Why this exists

[docs/65](65-api-surface-coverage.md) closed this gap for the machine-written
JSON under `docs/`: every evidence ledger in that directory now has a chapter
naming it. The decay it argues against is one directory over, and in a sharper
form.

A ledger under `docs/` at least leaves a reviewable artifact. These 17 gates
mostly do not. **Fourteen of them write nothing at all** — their baselines live
in their own source, as a `WAIVED` dict, an `UNDOCUMENTED` set, a
`SINGLE_SOURCED` map, a `MAX_AGE_DAYS` constant. There is no file to open. The
only route to "what does this gate actually assert, and what would I change to
accept a new case" was to read the checker, which is precisely the state
[`scripts/check_doc_drift.py`](../scripts/check_doc_drift.py) argues against in
its own `make check` comment:

> Prose is the one artifact here with nothing underneath it: a rename breaks a
> Go import loudly and the same rename in a sentence breaks nothing at all.

The inverse holds too, and it is this chapter's subject. A gate with no prose
above it breaks nothing when its meaning drifts. It keeps printing its success
line, and the question it answers quietly stops being the question anyone
thought it answered — [docs/10](10-testing.md#the-failure-this-codebase-keeps-producing)'s
item nine, applied to the guards themselves.

### How to re-derive the gap

Nothing gates this chapter's completeness, so the measurement is printed rather
than asserted:

```bash
for f in scripts/*.py scripts/*.sh; do
  b=$(basename "$f")
  n=$(grep -rlF "$b" docs --include='*.md' | grep -v '^docs/release-notes/' | wc -l)
  [ "$n" -eq 0 ] && echo "$b"
done
```

The denominator is `scripts/*.py` and `scripts/*.sh` — 61 Python files and 7
shell scripts. [`scripts/check_script_test_coverage.py`](../scripts/check_script_test_coverage.py)
reports 61 because it scopes itself to the Python half, which is the half it can
require a `python/tests/` module for; the two figures measure different sets and
neither is stale.

Release notes are excluded for the reason
[`scripts/check_doc_drift.py`](../scripts/check_doc_drift.py) skips them: a note
describes the tree *at its tag*, so a script named only there has a historical
mention and no current explanation. That is the same argument
[docs/65](65-api-surface-coverage.md#why-this-exists) makes about the three REST
ledgers, and it is why this page exists for the same reason that one did.

## The inventory

| Gate | What it reads | What it asserts | Where it runs | Artifact |
|---|---|---|---|---|
| `scripts/check_adf_activity_types.py` | `third_party/adf-pipeline-schema/Pipeline.json` | every concrete ADF activity type is dispatched, interpreted or refused by name — never the success stub | `make check` | none |
| `scripts/check_fabric_activity_types.py` | `third_party/fabric-activity-types/activity-types.json` | the Go activity-type list equals Microsoft's vendored table, exactly, both directions, plus the sha256 pin | `make check` | none |
| `scripts/check_refusal_expectations.py` | `e2e/conformance/live.py`, `third_party/azure-storage-error-codes/models.py` | every contract-8 refusal expectation has a second source, or is recorded in `SINGLE_SOURCED` with why it has none | `make check` | none |
| `scripts/check_runtime_floor_freshness.py` | `e2e/conformance/fabric-runtimes.json` | every runtime entry carries its source page and a read date no older than `MAX_AGE_DAYS` (183) | `make check` | none |
| `scripts/check_runtime_wiring.py` | `python/notebookutils`, `python/spark_agent` | every env var and runtime-context key the shim or agent *reads* is *set* somewhere that is not the reader and not a test | `make check` | none |
| `scripts/check_endpoint_env_names.py` | `python/fabric-target/fabric_target/__init__.py` | every endpoint-shaped override a harness sets names a variable fabric-target actually reads | `make check` | none |
| `scripts/check_e2e_matrix.py` | `e2e/`, `docs/12-e2e-matrix.md` | every e2e suite is described in the matrix, or named in `UNDOCUMENTED` (14 entries) | `make check` | none |
| `scripts/check_docs_sidebar.py` | `docs/`, `website/astro.config.mjs` | every published doc is reachable from the explicit Starlight sidebar | `make check` | none |
| `scripts/check_docs_links.py` | `docs/`, `website/astro.config.mjs` | no intra-doc Markdown link points at a missing page; no published page is missing from the sidebar | `make check`, `make docs-build` | none |
| `scripts/check_workflow_concurrency.py` | `.github/workflows/` | `ci.yml` and `make-targets.yml` agree on concurrency; every job declares a timeout; every workflow declares top-level permissions | `make check` | none |
| `scripts/check_mlflow_unpublished.py` | `e2e/data-science-loop/docker-compose.yml` | the MLflow container publishes no port to the host | `make check` | none |
| `scripts/check_capture_redaction.py` | `scripts/capture_definition_shape.py` | the redactor lets nothing through from a definition built entirely of things that must not leave a tenant | `make check` | none |
| `scripts/check_entra_install.py` | `e2e/entra_install.py` | the install retry fires on sumdb lag and *not* on a broken pin or a compile error; a PATH hit is a binary the harness can terminate | `make check` | none |
| `scripts/check_port_guard.py` | `e2e/`, `e2e/port_guard.py` | the guard fires, names the process holding the port, and no harness has grown its own copy again | `make check` | none |
| `scripts/check_kind_exhaustiveness.py` | `internal/store/bus.go`, `portal/src/eventKinds.ts` | declaring a new event kind in Go **breaks the portal's TypeScript build** | `make portal-types` | mutates and restores `internal/store/bus.go` |
| `scripts/gen_sbom.py` | `go.mod`, every tracked `uv.lock`, `pnpm-lock.yaml` | a CycloneDX inventory can be built from committed lock data alone; exits 1 if a present manifest yields nothing | `make sbom` | `sbom.cdx.json` |
| `scripts/check_advisories.py` | every tracked `uv.lock`, `pnpm-lock.yaml` | no locked version carries an open advisory that is not held in `docs/advisory-holds.json` | `.github/workflows/security.yml` | none |

**Thirteen of the seventeen also run as their own step in the `witnesses` job**
in `.github/workflows/ci.yml`, so a local `make check` and CI reach them by two
independent routes. `scripts/check_kind_exhaustiveness.py` has the same double
route, through CI's `portal-types` job.
`scripts/check_workflow_concurrency.py` is the one `make check` gate with no
step of its own: it reaches CI only through `make test` in
`.github/workflows/make-targets.yml`, which depends on `check`. The three
mentions of it in `ci.yml`, `.github/workflows/codeql.yml` and
`make-targets.yml` are comments pointing at it, not invocations — so a grep for
its name in the workflows reads as wiring that is not there.

**None of the seventeen takes an update flag.** That is worth stating plainly,
because it is the difference between this family and
[docs/65](65-api-surface-coverage.md)'s: there is no baseline to regenerate and
no diff to review. Accepting a new case means editing a named constant in the
checker — `UNDOCUMENTED`, `WAIVED`, `SINGLE_SOURCED` — in the commit that
argues for it, and the edit *is* the review. Three offer a strict mode as a
separate switch (`scripts/check_docs_links.py`,
`scripts/check_e2e_matrix.py`, `scripts/check_runtime_wiring.py`), and that is
the full list; the rest exit non-zero unconditionally, so there is no
report-only mode to leave on by accident.

## Offline source gates

Four read Go or Python source and a vendored oracle, and answer before a push.

`scripts/check_adf_activity_types.py` and
`scripts/check_fabric_activity_types.py` are the same defect from two sides. The
emulator accepts ADF's activity vocabulary beside Fabric's own, and an
unrecognised type falls to the dispatch default and is reported as having
succeeded while doing nothing — the defect that hit twelve Fabric types. The
Fabric half holds the hand-transcribed Go list to Microsoft's vendored table in
both directions plus a sha256 pin. The ADF half checks that every concrete type
is handled, where "handled" means a case in `internal/api/pipelines.go`, a case
in `internal/pipeline/activities.go`, or a key in
`internal/api/unrunnableactivities.go` — the checker does not care which. Its
best detail is that **abstract bases are excluded by derivation, not by
exemption**: the two base definitions carry discriminators but are inherited
from, and the checker finds them by walking the schema's `allOf` and asking
which definitions are someone else's base. An exemption list there would be the
thing the file exists to delete.

`scripts/check_runtime_wiring.py` names every environment variable and
runtime-context key the notebook runtime *reads* that nothing in the product
*sets*. Four defects had that shape: the session shim read `SPARK_AGENT_URL` and
`FABRIC_SESSION_ID` from an environment nothing populated, so both raised "this
is running outside a notebook session" inside one, with unit tests green
throughout because the tests set the variables. It is honest about its
asymmetry: finding readers is an exact AST walk, finding *writers* is not — a
value can arrive from compose, a Dockerfile, Go, or a shell script — so the
writer patterns are deliberately narrow, and a name appearing only in prose or a
comment does not count as set. Four names are waived with reasons, and strict
mode fails on a waiver that is no longer needed.

`scripts/check_endpoint_env_names.py` is the same question at the harness
boundary. Three callers kept setting pre-move names that nothing on the
resolution path reads, and the defaults are exactly the ports CI publishes, so
an override that did nothing looked like one that worked. It mattered for
`docs/demo/flow-override.yml`, which remaps every published port and wired the
remap with the one variable being ignored. The accepted set is read from
`python/fabric-target/fabric_target/__init__.py` itself, so adding an alias
there cannot make the check stale.

## Vendored-oracle and transcription gates

Two guard a hand transcription of somebody else's contract, which is the
failure mode with no natural second opinion.

`scripts/check_refusal_expectations.py` grades contract 8's refusal
expectations against a source that is not our own typing. **Its failure
direction is a false green**: if Microsoft renames a code, the map keeps
asserting the old one and the probe keeps passing — and contract 8 exists to
catch the permissive direction, so a stale expectation blinds the probe built to
see. Each case declares what backs it, and the answer differs per case: the ADLS
Gen2 wire code comes from Microsoft's own SDK enum, vendored under
`third_party/azure-storage-error-codes/` rather than imported because every
guard runs stdlib-only; the POSIX errno comes from CPython, by running the
operation. One case has no second source and says so in `SINGLE_SOURCED`, with
the reason — POSIX rename *replaces* the destination silently, so CPython
contradicts that expectation rather than corroborating it. **A new case with no
declared source is a failure, not an omission.**

`scripts/check_runtime_floor_freshness.py` is the one gate here designed to go
red on its own. Contract 3's runtime-floor comparison is monotone, which is
correct and is also the problem: an upgrade keeps passing, so nothing about it
will ever go red by itself, and a raised Microsoft floor would leave the
emulator advertising a stale version with the probe still approving. Fabric
publishes runtime versions as documentation pages and nothing machine-readable,
so there is no oracle to vendor. **The claim is bounded in time instead**: every
entry in `e2e/conformance/fabric-runtimes.json` carries the page it came from
and the date somebody read it, and a reading older than 183 days fails. This
check will eventually fail without anybody changing the code. That is the
design.

## Documentation and workflow-config gates

`scripts/check_docs_sidebar.py` and `scripts/check_docs_links.py` overlap on
the sidebar deliberately, and the newer one says so in its own header. Both
matter for the same measured reason: **the Astro build exits 0 on a dangling
intra-doc link and publishes a 404**, and Starlight's sidebar here is an
explicit list in `website/astro.config.mjs`, so a doc left out still builds,
still gets a URL, still appears in search, and is simply unreachable by
navigation. Nothing fails.

Both are worth reading for one shared design decision. What counts as published
is decided by a single regex in `website/scripts/sync-docs.mjs`, and **both read
that regex rather than copying it.** The sidebar checker records why: it used to
hold a copy under a comment telling a future reader to keep the two in step, and
the test that claimed to pin the coupling asserted only that the regex's *name*
still appeared in the `.mjs`, then compared the local copy against a
hand-written list of filenames. Edit the pattern to publish a different set and
everything stayed green — the assertion matched a phrase that co-occurs with the
claim rather than the claim itself. That is
[docs/10](10-testing.md#the-failure-this-codebase-keeps-producing)'s item nine
in miniature, and the fix was to delete the second list rather than guard it.

`scripts/check_e2e_matrix.py` holds `docs/12-e2e-matrix.md` to the suites that
exist, and was found by writing a suite and forgetting to register it —
`e2e/notebook-display` shipped, ran in CI, and was invisible in the index a
reader uses to answer "what is actually proven end to end". **The backlog is
recorded, not waived**: fifteen suites were already missing when it landed, so
listing them as fine would have made the check a formality on day one. Fourteen
remain in `UNDOCUMENTED`, each with what its absence costs, and strict mode
fails on a name that no longer needs to be there.

`scripts/check_workflow_concurrency.py` holds three invariants across
`.github/workflows/`, and its header is the clearest statement in this tree of
when a checker is the *right* tool. Deriving the coupling is impossible — GitHub
Actions has no cross-file include and YAML anchors do not span documents.
Collapsing it is impossible — every workflow must carry its own concurrency
block. So this is the residual case: two independent artifacts that must merely
agree, which is what a checker is for, and reaching for one where derive or
collapse was available would be the mistake. The second invariant is that every
job declares a timeout, because GitHub's default is **six hours** — a Sail job
sat in progress for 78 minutes while a merge watcher waited, since "running" and
"wedged" are the same status to a checks query. The third is a top-level
permissions block per workflow, found by CodeQL's actions pack against
`.github/workflows/codeql.yml`, whose `changes` job inherited the repository
default.

## Reachability, redaction and consolidation gates

Five assert a property somebody could delete in one line while debugging.

`scripts/check_mlflow_unpublished.py` is the sharpest, because the mitigation it
guards is the *only* one available. GHSA-h7x2-h6g9-p789 (high) is an SSRF in
MLflow's AI Gateway with **no patched version**, and downgrading is not
available either, because this project's floor is deliberately 3.15: a resolve
below it removed the security-middleware flag the e2e passes, and the container
would not start. Nothing here uses the AI Gateway, but the endpoint exists on
that container, so what keeps it harmless is that nothing outside the compose
network can reach it. The assertion is therefore narrow and exact: no published
port anywhere in `e2e/data-science-loop/docker-compose.yml`. Its failure message
says to re-assess the advisory rather than delete the check.

`scripts/check_capture_redaction.py` drives the redactor in
`scripts/capture_definition_shape.py` against a definition whose every leaf is
something a real tenant would rather not publish — a warehouse name, a blob URL,
a payroll query, a tenant GUID, an email, a finance path. This is a privacy
claim printed in a public log, so it is asserted rather than reviewed: a
redactor that passes one value through publishes a customer's hostname forever,
and the reviewer who would have caught it is reading a diff, not the output.

`scripts/check_entra_install.py` tests a retry **in both directions**, which is
the whole point: too narrow and it does not fire on the failure it was written
for, too broad and it turns a real breakage into three sleeps and the same error
with the diagnosis buried under a retry counter — and both look identical in a
green log. It pins the real checksum-lag message from the run that prompted it,
a broken-pin message of the same shape, a compile error, and a hypothetical
rewording, classifying on the structural fact (the error names a version *other*
than the pin) rather than the prose. Its second half is the PATH rule: a
version-manager shim runs the emulator as a child, so terminating reaches the
shim, the wait reports a signal, and teardown reports success while the emulator
holds its port until reboot.

`scripts/check_port_guard.py` asserts that the busy-port check fires, names the
process holding the port, and that **no harness has grown its own copy again**.
The count is the assertion, for a reason worth copying: the function existed
fourteen times, ten byte-identical, and the improvement that prompted the
consolidation into `e2e/port_guard.py` — naming the process instead of guessing
at Docker — would otherwise have had to be made fourteen times to be true.
Copies do not announce themselves; the next harness is written by copying the
last one, and a duplicate passes every test the original passes.

`scripts/check_kind_exhaustiveness.py` is the odd one out and the most
interesting. `scripts/gen_event_kinds.py` emits the event-kind type as a
TypeScript union so a portal switch that forgot a kind cannot compile — a claim
about a compiler, and a claim about a compiler that nothing exercises is a claim
nobody checked. A default arm, a stray escape to `any`, or strict mode quietly
switched off would each make the guarantee evaporate with every test still
green. So it **mutates `internal/store/bus.go` and requires the build to fail**,
twice: once for a kind the log must render, caught by a default arm that narrows
to `never`; once for a kind that is neither viewable nor dropped, caught by the
ingest path having nowhere to put it. Each mutation is bracketed by a clean run,
so a build failing for an unrelated reason cannot pass for the right one. It
needs Node and an installed portal, which is why it lives in `make portal-types`
rather than `make check` — `check` is deliberately runnable with nothing but
Python.

## Dependency and SBOM gates

`scripts/check_advisories.py` is the only one of the seventeen that
`make check` cannot reach, and its header explains the split. It needs the
network and a third-party binary, while the guards under `scripts/` are offline
invariants, so it lives in `.github/workflows/security.yml` — which runs on
push, pull request, a weekly cron *and* dispatch, so it is exercised on every
change rather than becoming a cron-only workflow nobody has run.

**Its inputs are discovered, never listed**: `git ls-files` decides what gets
scanned, because a hand-maintained path list is exactly the failure that left
seven example lockfiles watched by nothing. Every lockfile is scanned even after
one fails, and the per-lockfile package tally is printed on success — a scanner
that silently found no inputs reports the same green as one that scanned
everything. Its first run found seven advisories over 1592 locked packages, and
the one that justifies the file is a CVSS 8.2 in `cryptography` whose fixed
version was exactly the major release `.github/dependabot.yml` was holding the
package below, with a comment that had written the risk down in advance and
nothing asking whether it had materialised.

**What it is not**: a version-level scan against the advisory graph, not
reachability-filtered the way `govulncheck` is. A finding means "the locked
version is affected", not "this code calls the affected symbol", and that
residual gap is restated in `SECURITY.md` rather than papered over.

`scripts/gen_sbom.py` answers the question none of the scanners do — not "is
anything we depend on known-bad" but "what do we depend on", which is what a
consumer pulling the published image asks first. It reads **only resolved,
committed lock data**: `go.mod` requirements, every tracked `uv.lock`, and the
packages table of `pnpm-lock.yaml`. Never the network and never an installed
environment, so the output is identical on every machine. It is deterministic by
design — no timestamp, and the serial number is derived from the component list,
so regenerating from an unchanged tree is byte-identical and a diff of two SBOMs
is a diff of the dependencies. It does not resolve licences, which `SECURITY.md`
records as a known gap.

## The 13 that are not gates

Out of scope here, and named rather than quietly dropped. None asserts an
invariant; each is build, vendor or developer tooling, and a chapter explaining
`make check` is not where they belong:

| Script | Role |
|---|---|
| `scripts/assemble_site.py` | assembles the published site from the built landing page and docs (`make docs-build`) |
| `scripts/build_landing_data.py` | generates the landing page's data |
| `scripts/coverage_badges.py` | renders the coverage badges |
| `scripts/docker-smoke.sh` | smoke-tests a built image |
| `scripts/doctor.sh` | `make doctor` — toolchain and docker-context diagnosis |
| `scripts/port_conflict.sh` | prints the diagnosis when `make up` cannot bind |
| `scripts/fetch_spark_jars.sh` | fetches Spark jars for the JVM image |
| `scripts/fetch_connect_client_jars.py` | fetches the Spark Connect client jars |
| `scripts/probe_sail_merge_premise.py` | a one-off probe of a Sail behaviour |
| `scripts/spark_check.py` | a developer convenience against a running engine |
| `scripts/vendor_adf_pipeline_schema.py` | re-vendors ADF's pipeline schema |
| `scripts/vendor_fabric_activity_types.py` | re-vendors Microsoft's activity-type table |
| `scripts/vendor_openmetadata_schema.py` | re-vendors the OpenMetadata schema |

The three vendor scripts are the near miss. Each is the deliberate,
network-touching half of an offline gate above: re-vendoring the activity-type
table is what catches *upstream* drift, where
`scripts/check_fabric_activity_types.py` catches drift between the Go list and
the bytes already committed here. They are named for that reason and not
inventoried, because nothing in `make check` or CI runs them.

## Two counts in docs/10 that no longer match the tree

Found while reading, and documented rather than repaired — the precedent
[docs/65](65-api-surface-coverage.md#the-inconsistency-this-document-found) set
with `docs/route-coverage.json`. Regenerating or editing a reviewed number
belongs in a change that can show the diff.

[docs/10](10-testing.md#one-level-in-who-tests-the-guards) states that
`make check` runs thirty-one of these scripts and the `witnesses` job runs
thirty. Measured against the tree at this commit, **each runs 37** — 35 of them
named `check_*`. Both figures were true when written; both have aged in the safe
direction, understating the enforcement rather than overstating it.

The same sentence pair is worth one more line, because the two sets are *not*
identical and nothing says which is which. `make check` runs
`scripts/check_example_parity.py` and `scripts/check_workflow_concurrency.py`,
which the `witnesses` job does not; `witnesses` runs
`scripts/check_cask_stanzas.py` and `scripts/check_image_digests.py`, which
`make check` does not. Two each way, by design in each case, and recorded
nowhere.

Separately,
[docs/10](10-testing.md#the-docs-lane--what-a-documentation-change-runs)
describes `.github/workflows/make-targets.yml` as having **three jobs**; it
declares **two**, `changes` and `make-targets`. The claim that depends on the
count — that it runs on every change and is also the release gate, so a gate
that can be skipped is worth more care than three runner-minutes — is
unaffected.

## What this does not say

Stated plainly, for the reason
[docs/65](65-api-surface-coverage.md#what-none-of-this-says) gives: a document
listing only what it covers reads as complete.

- **It lands no new checker and changes no gate behaviour.** Every gate above
  already ran before this page existed, and runs identically after it.
- **Nothing gates this inventory's completeness.** There is no ratchet on
  script-documentation coverage, so a gate added tomorrow with no prose will not
  fail anything. That would be a second, larger change with its own ledger to
  review. The derivation command above is how a reader re-derives the gap
  instead.
- **Everything citable in this page *is* gated.** Every backticked repo path,
  every `make` target, every documented environment variable and every Markdown
  link here is checked by
  [`scripts/check_doc_drift.py`](../scripts/check_doc_drift.py) and
  [`scripts/check_docs_links.py`](../scripts/check_docs_links.py), and this
  page's presence in the sidebar is checked by
  [`scripts/check_docs_sidebar.py`](../scripts/check_docs_sidebar.py). So the
  *facts* decay loudly even though the *coverage* does not.
- **A gate's docstring is its author's account, not an assertion.** Where this
  page summarises a checker's reasoning it is reporting what that file claims.
  [docs/10](10-testing.md#the-failure-this-codebase-keeps-producing)'s item ten
  is exactly about prose asserting a contract the code does not hold, and
  nothing mechanical separates a true present tense from a false one.
- **It says nothing about the 38 scripts a prose page already names.** Those are
  explained where they are relevant — the four REST contract gates in
  [65](65-api-surface-coverage.md), the three flakiness ledgers in
  [60](60-test-flakiness.md), the drift and comment-drift checkers in
  [10](10-testing.md).
- **Presence is not strength.** Every gate here runs. Whether each would catch
  the violation it was written for is
  [`scripts/check_script_test_coverage.py`](../scripts/check_script_test_coverage.py)'s
  question, and [docs/10](10-testing.md#one-level-in-who-tests-the-guards)
  records that two checkers in this tree were measured wrong by someone driving
  them against a violation they had to catch.
