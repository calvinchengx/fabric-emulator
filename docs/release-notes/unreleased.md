# Unreleased (after v0.40.0)

Draft of what landed on `main` after the `v0.40.0` tag. Rename this file to
`v0.41.0.md` (or whichever minor) when tagging. Open pull requests are not
here.

## Documentation drift catches Markdown links

`scripts/check_doc_drift.py` now checks ordinary Markdown links to tracked repo
files, directories and simple heading anchors, while still skipping historical
release notes and non-heading fragments such as line anchors. The first run
fixed stale links in the medallion PySpark README and old anchors into the real
compute and notebook capability docs. [docs/10](../10-testing.md#the-failure-this-codebase-keeps-producing)

## Doc drift slugs headings that contain links correctly

A heading such as `## See [Foo](bar.md) now` is slugged by GitHub as
`see-foo-now`. The Markdown-link check kept the URL in the slug, so a correct
anchor into such a heading would have been reported as dead.

A heading that contains an image, `## ![logo](l.png) Title`, is slugged as
`title`: an image contributes no text, so its alt text is no longer part of the
slug.

## A guard that every item type has a typed collection

Every documented `ItemType` is reachable at a typed REST collection today, but
nothing pinned it: a type added to the enumeration without a segment would 404
at the URL the reference prints while the generic surface created it fine. A
test now fails naming the type, and also fails on an alias whose type is not
documented.

## A WITH clause no longer hides a write from Warehouse versioning

`WITH c AS (…) INSERT/UPDATE/DELETE/MERGE …` was read as a query, so a Warehouse
table changed that way got no time-travel version, and an `INSERT` or `MERGE`
was recorded in lineage as `SELECT INTO`. The clause is now read through to the
verb after it; a write through a CTE (`WITH c AS (SELECT … FROM dbo.t) DELETE
FROM c`) is attributed to the base table. [docs/35](../35-warehouse-time-travel.md)

## Direct Lake applies OneLake row filters

A Direct Lake on OneLake query over a table a OneLake security role filters by
rows used to be refused, because nothing evaluated the predicate. It is now
applied: the caller gets the rows the filter admits. The filter is parsed by the
same code the SQL analytics endpoint renders into SQL Server predicates, and a
witness against a real SQL Server requires the two to return the same rows —
text compared case-insensitively, NULL comparisons admitting nothing. A filter
the emulator cannot apply blocks the table by name rather than serving it
unfiltered. [docs/54](../54-onelake-security.md)

## Row and column security from different roles no longer grants everything

A principal in one OneLake role that filters a table's rows and another that
narrows its columns read the whole table: consolidating the roles by union
opened both restrictions. Fabric does not support that combination and gives a
query error. Direct Lake and direct OneLake reads now give that error, by name;
`principalAccess` gives an engine both restrictions; the SQL analytics endpoint
shows such a reader no rows. [docs/54](../54-onelake-security.md)
## Fabric IQ MCP

The emulator now serves Microsoft's Fabric IQ MCP server at
`POST /v1/mcp/fabriciq`: the six read-only tools an agent uses to find a Power BI
report or semantic model, read its pages, visuals, filters and schema, look up an
exact stored value, and run DAX. Each tool runs as the signed-in user and needs
only Read on the item, and the model's row- and object-level security decide what
they see. Service-principal tokens are refused, as Microsoft documents. Report
definitions in PBIR and PBIR-Legacy form are now read for this, and the DAX
evaluator gained `ORDER BY`. Microsoft does not publish the tools' schemas; the
argument names and response shapes follow its own Fabric IQ skill.
[docs/07](../07-control-plane-api.md#fabric-iq-mcp)

## The REST surface ledgers are explained

Three evidence files CI rewrites on every run — `docs/surface-ledger.json`,
`docs/route-coverage.json` and `docs/undocumented-routes.json` — were named by
no prose page, so their numbers were reviewable only by reading the checker that
wrote them. A new chapter states what each one counts, what its denominator is,
how it ratchets, the exact command that regenerates it, and which gates need a
recording and so cannot answer before a push. It also reconciles the two
operation denominators in the tree: the parity map's ~880 from Fabric's
published reference against the ledger's 1002 from the vendored swagger, which
differ because the ledger also counts Power BI's surface and can only count
specs committed here.

Writing it found two ledgers publishing counts no gate compares.
`docs/undocumented-routes.json` records 613 registered routes where the tree now
has 1059 — one honest snapshot that aged, and its six-route invariant still
holds. `docs/route-coverage.json` is the sharper case: its `exercised`,
`registered` and `notYetExercised` cannot all be true, since its own writer
defines `exercised` as `registered` minus the listed routes, which is 113 and
not the 116 recorded. The gate reads only the route names, so the file has been
internally inconsistent and green at once. Both are documented rather than
repaired — regenerating a reviewed artifact belongs in a change that can show
the diff. [docs/65](../65-api-surface-coverage.md)

## Fabric Data Warehouse MCP

The emulator now serves Microsoft's Data Warehouse MCP server at both of its
endpoints: `POST /v1/mcp/dataPlane/sqlEndpoint`, and one scoped to a single
item. Its one tool, `execute_query(workspaceId, itemId, query)`, runs a T-SQL
batch on a Warehouse or a lakehouse's SQL analytics endpoint as the signed-in
user, and returns the last result set as CSV. It takes the same path a TDS
client's batch does, so a Viewer and the endpoint's data are read-only, Fabric's
dialect applies, and SQL Server enforces the caller's grants and row-level
security. A write it runs is recorded for lineage as one sent over TDS is.
The tool is `execute_query`, as the live server lists it; the name on the Learn
page, `executeSQL`, works too. Its result is CSV embedded as a `text/csv`
resource, then a row count. A read-only Warehouse session's
refusal now says that, instead of naming the lakehouse endpoint.
[docs/07](../07-control-plane-api.md#fabric-data-warehouse-mcp)

## Eventhouse MCP

The emulator now serves Fabric's remote Eventhouse MCP server at both of its
endpoints: `POST /v1/mcp/dataPlane/kqlEndpoint`, and one scoped to a single KQL
database. It has the four tools Fabric's live server lists: `executeQuery` runs
real KQL on the attached engine as the signed-in user, and `getSchema`,
`getGeneralKQLExamples` and `getSpecificKQLExamples` ground a query in the
database. Microsoft names none of these tools; the contract comes from two
third parties' captures of the live server, and the grounding documents are the
emulator's own. [docs/07](../07-control-plane-api.md#eventhouse-mcp)

## e2e harnesses no longer leave entra-emulator running

On a machine where a version manager such as goenv puts `entra-emulator` on
PATH, every e2e harness that starts it left it running after the run, holding
its port, and the next run refused to start. PATH resolved to a shell-script
shim, which runs the real binary as a child process, so the harness's
shutdown stopped the shim and not the emulator. `e2e/entra_install.py` now
uses PATH only when it finds the binary itself. For a wrapper script it takes
the binary `go install` put in GOBIN, and if there is none it installs its own
copy. The fifteen copies of the busy-port check are now one, `e2e/port_guard.py`,
and when a port is taken its message names the process holding it and says
whether that process outlived the run that started it.

## Test cases can be kept as data

A suite whose cases are a table can now keep them in `cases/<suite>.json`, read
by every runner that executes them: a pytest test marked `cases("<suite>")` is
parametrized over the file, and an e2e runner reads it through the
standard-library `scripts/casefiles.py`. `make check` refuses a case without a
unique kebab-case `id` or a `why`. The spark agent's consumer contract is the
first suite: the unit test and `e2e/agent-contract` now read the same twelve
cases, where the e2e runner used to retype the statements it executed. Fabric
IQ MCP is the second: its Go test and its e2e driver, which calls the tools
through the unmodified `mcp` SDK as entra-emulator's seeded users, now run the
same nineteen tool calls from `cases/fabric-iq-tool-calls.json`. Each case's
expected answer is a short list of paths into the tool's JSON reply, read the
same way in Go and Python.
[docs/10](../10-testing.md#test-cases-as-data)

## SQL analytics endpoint access modes can be switched

A lakehouse's SQL analytics endpoint now has a data access mode, delegated
identity by default — what it always did. An Admin or Member switches it through
the emulator-native `…/sqlEndpoints/{id}/_emulator/dataAccessMode` (Fabric offers
no API), and the switch applies Fabric's documented effects: the workspace's SQL
sessions end, and SQL roles, security policies and functions change as the mode
requires. In user identity mode, the lakehouse's OneLake security roles are
synced into the endpoint as `OLS_` database roles and row policies: a Viewer
reads only the tables, columns and rows their roles grant, a role member of any
workspace role is held to its row filter, and T-SQL cannot grant tables around
them. Direct Lake on SQL over such an endpoint returns each caller the same
rows, and `directLakeOnly` fails there, as Fabric's always falls back.

The endpoint also accepts the SQL objects and security authored on it — views,
functions, roles, grants, security policies, masks — which the relay refused as
writes before; data writes stay refused, now including one after a leading
block comment or after another statement in the batch.
[docs/60](../60-sql-endpoint-access-modes.md)

## Shortcuts on the SQL analytics endpoint

A OneLake, ADLS Gen2, Amazon S3 or Dataverse shortcut under `Tables/` now reads as a table on a lakehouse's SQL analytics endpoint, from its target: it was not reflected at all. For a OneLake source, in user identity mode the source's security applies as Fabric documents — a caller the source refuses is refused, and its column allow-list and row filters narrow the read on top of the consumer's own roles, the intersection winning — and in delegated mode a shortcut whose source has row or column security is blocked for every reader, as a view that refuses with the reason. An external target carries no OneLake security, so neither applies to it, correctly. Not built: ownership chaining. Where a row filter reads a column that another reader is denied, that reader is refused the table, because SQL Server needs SELECT on a policy's predicate columns; this also affects a table with no shortcut when one role narrows columns and another filters rows. [docs/61](../61-sql-endpoint-shortcuts.md)

## Delegated mode blocks a shortcut whose source is secured

On a SQL analytics endpoint in delegated identity mode, a shortcut whose source table has row-level or column-level security is now **blocked**, as Fabric documents: the endpoint reads OneLake as the item owner, who can only read a table whole. Every reader is refused, an Admin or Member included, with a message naming the reason; it is reflected as a view that refuses every read. Switching the endpoint to user identity mode lifts the block, where the caller's own identity is checked at the source instead. Fabric documents that access is blocked but not what a client sees, so the message is ours. [docs/61](../61-sql-endpoint-shortcuts.md)

## Copy activity: `fileListPath` (list-of-files copy)

A Copy activity source naming `fileListPath` now copies exactly the files that
text file lists — one relative path per line, each "the relative path to the
path configured in the dataset" (ADF's own wording) — instead of failing
loudly as every other unimplemented Copy option still does. A file present
under the source folder but left off the list is not copied; a listed file
that does not exist fails the activity rather than silently copying fewer
files than the list promised. The list file itself is addressed the same way
`folderPath`/`fileName` are. `fileListPath` on a **sink** is still refused by
name, since Fabric's schema carries it only on `*ReadSettings`. A list entry
that climbs out of the source path (`../x`, `/../x`) fails the activity and writes
nothing.
[docs/parity.md](../parity.md)

## Strict mode refuses two more of Fabric's unsupported T-SQL

`-tsql-strict` (`FABRIC_TSQL_STRICT`, off by default) now also refuses `FOR JSON` inside a subquery, derived table, CTE or call — Fabric allows it only as the last operator — and a `/` or `\` in the name of a schema or table being created. Of the T-SQL Microsoft lists as unsupported, strict mode now refuses 12 of 16, and together with the endpoint's write guard 14; the vector type is the one neither refuses, because the SQL Server 2022 sidecar has no such type. Nothing changes without the flag. [docs/29](../29-tsql-parity.md)

## A OneLake security role naming a missing column fails the sync loudly

A row filter or column allow-list that no longer matches its table's schema used to silently narrow that role's grant to nothing — a Viewer in the role simply read less than intended. Fabric documents a louder reaction: "Row-level security policy references a column that no longer exists. Database enters error state until policy is fixed." (the same sentence for column-level security). The sync now fails with that message when this happens, for a role's own constraint and for a OneLake shortcut source's role alike, so every read through the endpoint fails until the role is fixed at its source — not only the affected role's. An RLS filter that is merely invalid syntax (Fabric's separate, documented "no rows being shown" case) still narrows to nothing rather than failing the sync; the two are now kept distinct. [docs/60](../60-sql-endpoint-access-modes.md)

## The enforcing scripts are inventoried

Of the 68 scripts under `scripts/`, 30 were named by no prose page outside the
historical release notes, and 18 of those are wired, enforcing gates — 14 run
from `make check`, one from `make portal-types`, one from `make sbom`, the
advisory scan from `security.yml`, and the container smoke test from its own
`ci.yml` job. Unlike the REST ledgers docs/65 explained, sixteen of the
eighteen write no artifact at all, and fourteen of those keep their baseline in
their own source as a `WAIVED` dict, an `UNDOCUMENTED` set, a `SINGLE_SOURCED`
map, a freshness constant. There was no file to open, so each gate's
denominator, what it asserts and how a new case is accepted were reviewable
only by reading the checker.

A new chapter states those four facts for each of the eighteen, groups them by
what they guard — offline source, a vendored oracle, documentation and workflow
config, reachability, dependencies, the container — and names the 12 build and
vendor scripts it leaves out rather than dropping them silently. What makes a
script a gate here is that something fails when it fails, not which file
invokes it: that is why the advisory scan and the container smoke test are in
even though no `make` target reaches either. It lands no new checker and
changes no gate; the inventory's own completeness is gated by nothing, so the
one-line command that re-derives it is printed in the page.

Writing it found two stale counts in docs/10. It says `make check` runs
thirty-one of these scripts and the `witnesses` job thirty; each now runs 37,
and the two sets differ by two scripts in each direction, which nothing
records. It also describes `make-targets.yml` as having three jobs where it
declares two. Both are documented with each source named rather than edited, on
the precedent docs/65 set with `docs/route-coverage.json`.
[docs/66](../66-gate-inventory.md)

## `make doctor` names the two Windows docker failures

The named-pipe error — `open //./pipe/dockerDesktopLinuxEngine: The system
cannot find the file specified` — is what the docker CLI prints when it reached
no daemon at all, so it arrives from whichever command ran first and mentions
neither the image nor the cause ([#650](https://github.com/calvinchengx/fabric-emulator/issues/650),
reported against `docker pull`). `make doctor` already failed on an unreachable
context, but its advice was `docker context use <name>`, which is the fix for a
vendor clash and no help on a machine whose one context is Docker Desktop's: it
now recognises that endpoint and says the engine is stopped. The second failure
had no detection at all — Docker Desktop in **Windows containers** mode is
reachable and healthy, so the report read `ready — run: make up`, and the stack
failed two commands later with `no matching manifest for windows/amd64`. The
engine's `OSType` is now a blocker, with the switch to run.

The unreachable report also stopped guessing. It prints the **endpoint** it
could not reach, which is the one string that matches the error the user saw;
it says there is nothing to select when the failing context is the only one
configured; and it **probes** each alternative instead of listing names, so a
context is offered only when it answered and serves linux containers — one that
is as stopped as the first, or that answers while serving windows containers,
is labelled rather than recommended.
[docs/26](../26-platform-setup.md#the-named-pipe-error)

## CI image builds survive a registry outage instead of failing on it

On 2026-10-25 four e2e jobs failed inside `docker build` for reasons outside
this repository. `mirror.gcr.io` — the Docker Hub cache every Dockerfile here
pulls through since #556 — served `golang:1.27`'s manifest while answering
`not found` for one of its layers for about two hours, and `proxy.golang.org`
reset the HTTP/2 streams of four modules mid-`go mod download`. The mirror
reports a failed refill as a 404, which BuildKit treats as final, so nothing
inside the build ever retried; and the governance harness's existing
three-attempt `compose up` retry failed all three times, because 15s and 30s
of backoff cannot outlast a two-hour window.

Every job that runs an e2e harness now warms the base images that harness will
build before it builds them, via `scripts/warm_base_images.py --suite <name>`:
a bounded retry for a short failure, then a **second source** for a long one —
the same reference from Docker Hub, tagged under the mirror's name, which is
the same content-addressed image. With it in the local store BuildKit builds
without contacting the registry at all. The step reads each suite's own compose
files, so the 17 suites that build nothing pay a parse and no pull, and a test
asserts every harness job has it, Linux-guarded, directly after checkout. The
root `Dockerfile`'s `go mod download` is retried four times and still ends the
build when every attempt fails.

## The CI warm-up covers pulled service images, and the jobs it missed

#666 pre-pulled the base images each harness BUILDS. Two of the same morning's
failures were compose PULLING a service image — SeaweedFS for e2e/s3, ttyd for
e2e/portal-terminal — and died on `unknown blob`, the same mirror failing
through a different client. `scripts/warm_base_images.py` now warms those too:
every `image:` in a suite's own compose files, and an `image:` from the root
`docker-compose.yml` only when the suite layers that file and names the service
it starts. That keeps OpenMetadata, OpenSearch and Kafka out of suites that
never run them, and keeps e2e/delta-rs — which names `fabric-emulator` while
running it as a host binary — from being handed the published image. Compose's
default `pull_policy: missing` then finds each one local and does not pull.

Two jobs had no warm step at all, because #666 recognised only literal harness
paths: the medallion matrix (`e2e/${{ matrix.harness }}/run.py`), one of the four
jobs that change was written for, and the coverage job's shell loop. Both are
wired now, and the test asserting every harness job warms first reads all three
forms. Three medallion harnesses hold no stack of their own and run
e2e/medallion's; they are now followed to it, as e2e/two-context is to e2e/livy.

## The warm-up reaches the cron and release workflows, and images a harness runs itself

`spark-jvm`, `xmla` and `sempy` — cron/dispatch-only workflows — and
`release.yml`'s `agent-contract` job run e2e harnesses and had no warm step: the
invariant asserting every harness job warms first read ci.yml alone. It now
reads every workflow, with two exemptions recorded with their reasons:
`real-fabric`'s conformance job targets real Fabric and starts no local stack,
and `pbix-desktop` runs on Windows. A job that warms may not also switch to a
buildx builder, since that keeps its own image store and the warm-up could not
reach it — which is why `release.yml`'s multi-arch `image` jobs are left alone.

Two harnesses run containers outside compose, and warmed nothing until now:
xmla `docker run`s the .NET 8 SDK from a module constant, and sempy builds
`e2e/sempy/image/Dockerfile`, below the suite's top level. Whole-literal image
references (with a registry host and a tag or digest) and nested Dockerfiles
are now read, which also brings kustainer into mcp-eventhouse's warm set. An
implicit Docker Hub reference with no host — the `alpine/socat` forwarder — is
not, being indistinguishable from any other `a/b` string.

## The python base image is held at 3.14 until zstandard ships a cp315 wheel

`python:3.15-slim` cannot build this repository's lockfile. pyspark-client
4.2.0 requires zstandard, and zstandard 0.25.0 — the newest release — ships
wheels up to cp314 only, so on 3.15 `uv sync` falls back to the sdist and the C
extension build fails inside the image. The bump
([#656](https://github.com/calvinchengx/fabric-emulator/pull/656)) took 28
containerized jobs down with it, and `docker/sail` and `docker/spark-agent` are
images this repository PUBLISHES, so every platform in the family would have
inherited one that cannot be built. Minor bumps of `library/python` are now
held with a `LIFT THIS` naming the upstream change that retires the hold —
minors rather than 3.15 alone, because 3.16 fails the same way until the wheel
exists, while 3.14 patch updates still arrive inside the tag.

## A portal or front-end dependency change no longer runs the e2e stacks

A pull request confined to `portal/`, the pnpm workspace files and
documentation now runs the Go test matrix and the four jobs that can observe the
portal, instead of all ~110 checks. On 2026-10-25 about half the pull requests
were of this kind, and the account's runners were the bottleneck: at points one
job ran with 24 runs queued behind it. `portal/dist` is embedded in the Go
binary, so the Go tests still run on every OS; `portal`, `portal-types` and
`portal-terminal` read `portal/` directly. Every commit on `main` still runs
everything.

Dependabot no longer fires every ecosystem on the same day — Go on Monday
through GitHub Actions on Friday — and npm's minor and patch updates arrive as
one weekly pull request rather than one each. Seven separate npm pull requests
arrived that way on 2026-10-25; majors stay individual.

## No co-authorship trailers on main, checked rather than trusted

GitHub's generated squash message appends a `Co-authored-by:` line for every
commit author on a pull request's branch, and six squash commits on `main`
(#639, #641, #642, #655, #663, #664) landed with one that none of their branches
contained. entra-emulator's attribution check is now ported here: a workflow
that fails a pull request whose commits or description carry `Co-authored-by:`
or `Claude-Session:`, reruns when the description is edited, and on every push
to `main` checks only the commits that push landed — so it does not re-flag the
six, which are left in place. `make merge PR=N` squash-merges with an explicit
message built from the title and description, refuses if the branch moved since
it was read, and reads the landed commit back to fail loudly if GitHub added a
trailer anyway. A Dependabot merge stays authored by `dependabot[bot]`.
