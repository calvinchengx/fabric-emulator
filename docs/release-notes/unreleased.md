# Unreleased (after v0.35.0)

Draft of what landed on `main` after the `v0.35.0` tag. Rename this file to
`v0.36.0.md` (or whichever minor) when tagging. Open pull requests are not
here.

The range starts at `2f95c3f5` (#416), the first commit after the `v0.35.0`
tag: 38 changes. The headline is a **Databricks JAR task that actually
submits**, and two review findings that reached main after it.

**Read the first section if any Databricks JAR task used to be refused with
"nothing here submits a main class".** That refusal was true of the statement
endpoint and false of the overlay.

**Read the second if a retry of a failed Environment pip install came back
`applied: true`.** The cache recorded the attempt, not the result.

---

## A JAR task runs on the JVM overlay, refused where it cannot (#438)

The refusal said nothing here submits a main class, on either engine. The first
half was true of the statement endpoint; the second was a gap — the overlay
ships `spark-submit`. Both survivors are probed, not assumed. A jar runs from
the Files mount or not at all: the mount is enumerated and the request only
selects from it, so traversal and symlinks match nothing rather than being
caught after the fact.

## A failed Environment pip install was recorded as applied (#453)

A `/environment` call that failed still entered `_environment_applied`. The
next bind of the same Environment short-circuited as already installed, so a
retry of a broken `requirements.txt` looked like success. The cache now records
a completed install.

The same change restores `os.environ` around every `python/tests` case
(examples' fixtures wrote `NOTEBOOKUTILS_*` at import and the leak crossed a
package boundary) and refuses a JAR the Files mount cannot see.

## Databricks JAR `parameters` resolve against the pipeline scope (#454)

`parameters` were `fmt.Sprint`'d into argv, so an expression stayed a literal
and a non-string value became Go's default formatting. They now go through
`resolve()`, same as every other activity input. Notes in #455.

## The rest

**The MERGE intercept's reason had expired (#437).** pysail 0.7.1 plans the
shape the rewrite was written for, control included. The intercept still fires
until the `az://` case is proven; the justification now says what is true.

**A JVM Connect client measured against Sail (#443).** Handshake, SQL,
DataFrame ops, parquet and exit codes work; typed map, `sparkContext`, Scala
UDFs and `addArtifact` are asserted to refuse, so a pass fails the run as a
moved boundary.

**dbt silver without the Livy hop (#429, #431).** The same silver models over
Spark Connect: row counts agree with the Livy path. Sail's catalog is session
scoped, so the Connect path registers what it reads.

**fabric-cli is its own project (#426, #416),** so its pyyaml pin stops holding
the workspace. **The e2e MLflow server stays unpublished (#425)**
(GHSA-h7x2-h6g9-p789; nothing to upgrade to). Contract 8's expectations get a
source that is not our own typing (#434); contract 6 witnesses fall-through
directly (#433); contract 3's floor expires (#435).

**Dependencies.** tornado 6.5.8; examples take the root's pyarrow range;
great-expectations held at 1.x; Dependabot watches the seven example projects
and their lockfiles, and the published images. Plus the usual grouped bumps.

## Upgrading

No configuration changes. Consumers pin by digest, so bump
`FABRIC_EMULATOR_VERSION` and `FABRIC_EMULATOR_DIGEST` together.
