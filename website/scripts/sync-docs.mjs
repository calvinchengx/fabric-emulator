// Generates Starlight content from the canonical Markdown in /docs, keeping
// /docs as the single source of truth (its files stay pristine and their
// GitHub-relative links keep working). Run automatically before dev/build.
//
// For each docs/NN-name.md it: derives the title from the leading H1, injects
// Starlight frontmatter, drops the duplicate H1, and rewrites intra-doc
// `NN-name.md` links to site routes under the configured base.
import { readdirSync, readFileSync, writeFileSync, rmSync, mkdirSync, existsSync, statSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { collectParity, writeParityHistory, parityManifest, parityStats } from './parity-versions.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const REPO = join(here, '..', '..');
const DOCS_SRC = join(REPO, 'docs');
const OUT = join(here, '..', 'src', 'content', 'docs');
export const BASE = '/fabric-emulator/docs/';

// Parity version data (release tags + the live map), collected once. `version`
// is e.g. "v0.2.0" on a tag or "v0.1.0-69-g1935665" between releases.
const PARITY = collectParity(REPO);
const IS_RELEASE = /^v\d+\.\d+\.\d+$/.test(PARITY.version);
// The parity map is the one doc without a reading-order number: it is a living
// reference rather than a chapter, and its URL is just /parity/.
// Exact, for the same reason as in parity-versions.mjs: the loose form also
// matched `29-tsql-parity.md`, which then carried a version stamp describing a
// history it is not part of.
const PARITY_RE = /^parity\.md$/;
// Docs are `NN-name.md` chapters, plus the un-numbered living references
// (the parity map and the generated Spark engine matrix).
const DOC_RE = /^(\d{2}-.*|parity|engine-matrix)\.md$/;

// Rewrite `](./|docs/ NN-slug.md#anchor)` → `](/fabric-emulator/docs/NN-slug/#anchor)`,
// and the un-numbered `parity.md` the same way.
const LINK_RE = /\]\((?:\.\/|docs\/)?(\d{2}-[a-z0-9-]+|parity|engine-matrix)\.md(#[^)]*)?\)/g;

// Repo-relative links (`../docker-compose.yml`, `../e2e/fabric-cicd/`) are
// correct on GitHub, where /docs sits one level under the repo root — but they
// are dead on the site, whose pages are served from flat `/<base>/<slug>/`
// routes with nothing above them. Rewriting to an absolute GitHub URL is what
// keeps ONE source of truth working in both renderings, which is this script's
// whole premise; the alternative is editing /docs into something that no longer
// resolves on GitHub.
//
// `tree` vs `blob` is decided from what the path actually is on disk rather
// than guessed from a trailing slash, and a path that resolves to nothing is
// reported — that is how `../parity.md` was caught, a link that was broken on
// GitHub too (the parity map lives in docs/, not the repo root).
const REPO_URL = 'https://github.com/calvinchengx/fabric-emulator';
const REPO_LINK_RE = /\]\(\.\.\/([^)#]+)(#[^)]*)?\)/g;
function rewriteRepoLinks(md, where) {
  return md.replace(REPO_LINK_RE, (_m, path, anchor) => {
    const clean = path.replace(/\/+$/, '');
    const target = join(REPO, clean);
    const exists = existsSync(target);
    if (!exists) {
      console.warn(`sync-docs: WARNING ${where}: ../${path} matches nothing in the repo`);
    }
    const kind = exists && statSync(target).isDirectory() ? 'tree' : 'blob';
    return `](${REPO_URL}/${kind}/main/${clean}${anchor ?? ''})`;
  });
}

function rewriteLinks(md, where = 'docs') {
  const sitewide = md.replace(LINK_RE, (_m, slug, anchor) => `](${BASE}${slug}/${anchor ?? ''})`);
  return rewriteRepoLinks(sitewide, where);
}

// "02 — Emulated API surface" → "Emulated API surface".
function cleanTitle(h1) {
  return h1.replace(/^\d+[a-z]?\s*[—:-]\s*/i, '').trim();
}

// Backslashes must be escaped before quotes, or a title ending in one would
// escape the closing quote and produce unparseable frontmatter.
function yamlEscape(s) {
  return '"' + s.replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"';
}

// Strip the leading H1 (Starlight renders the frontmatter title) and rewrite
// intra-doc links. Shared with the parity snapshot generator so historical
// snapshots convert identically.
function convertBody(raw, where = 'docs') {
  const lines = raw.split('\n');
  const h1Index = lines.findIndex((l) => /^#\s+/.test(l));
  if (h1Index >= 0) {
    lines.splice(h1Index, lines[h1Index + 1]?.trim() === '' ? 2 : 1);
  }
  return rewriteLinks(lines.join('\n').replace(/^\n+/, ''), where);
}

// The context line at the top of the live parity map. Switching versions is the
// top-nav picker's job (src/components/ParityVersionPicker.astro) — this just
// says which version you're reading.
function parityStamp() {
  // On a release tag this reads "as of v0.2.0"; otherwise it's the moving tip,
  // "as of latest-b1e3520" — which says "unreleased tip" without pretending to
  // be a version.
  const what = IS_RELEASE ? `release **${PARITY.version}**` : `**${PARITY.version}** (the live tip of \`main\`)`;
  return (
    `_Parity map as of ${what} — tracked by git release tags. ` +
    `See the [version history](${BASE}parity-history/) and [parity changelog](${BASE}parity-history/changelog/)._\n\n`
  );
}

function convert(name) {
  const raw = readFileSync(join(DOCS_SRC, name), 'utf8');
  const h1 = raw.split('\n').find((l) => /^#\s+/.test(l));
  const title = h1 ? cleanTitle(h1.replace(/^#\s+/, '')) : name.replace(/\.md$/, '');
  let body = convertBody(raw, name);
  if (PARITY_RE.test(name)) body = parityStamp() + body;
  // Point "Edit this page" at the real source in /docs (the generated copy
  // under src/content/docs/ is git-ignored), not Starlight's default path.
  const editUrl = `${REPO_URL}/edit/main/docs/${name}`;
  const frontmatter = `---\ntitle: ${yamlEscape(title)}\neditUrl: ${yamlEscape(editUrl)}\n---\n\n`;
  return frontmatter + body;
}

function writeOverview() {
  // NO COUNTS HERE, deliberately. This page carried "113 supported capability
  // claims" long after the number was 120: a figure hardcoded in a generator
  // is a claim nothing checks, on the one page most likely to be read and
  // least likely to be re-read. The parity map owns the number and is
  // regenerated with it; this page owns the invitation to go and look.
  const body = rewriteLinks(
    `A local **Microsoft Fabric**: the control plane, OneLake, and engines that ` +
      `genuinely run your code. Workspaces, items and their CI/CD definitions, ` +
      `workspace RBAC, git integration, jobs and the 202/poll long-running-` +
      `operation contract — validating Microsoft Entra bearer tokens against ` +
      `[entra-emulator](https://calvinchengx.github.io/entra-emulator/) exactly ` +
      `as real Fabric validates against Entra. The same pipeline runs unmodified ` +
      `here and against a real tenant (\`FABRIC_TARGET\`).\n\n` +
      `**It computes.** \`docker compose up\` attaches Sail and a SQL Server ` +
      `sidecar: a notebook runs on Spark and writes Delta to OneLake, a warehouse ` +
      `query executes over a real TDS connection, and KQL runs on Microsoft's own ` +
      `Kusto engine. Microsoft's own tools drive it — \`fabric-cicd\`, the Fabric ` +
      `CLI, \`dbt-fabric\`, the Terraform provider, the VS Code extension — with ` +
      `no capacity and no cloud tenant.\n\n` +
      `:::caution\n**Local development tool only** — intentionally insecure (no ` +
      `real authorization boundary, self-signed TLS, seeded credentials). Run it ` +
      `on \`localhost\` only.\n:::\n\n` +
      `## The gap this is built around\n\n` +
      `Testing Fabric work against Fabric means a tenant, a capacity and a `+
      `cloud round trip for every iteration. A capacity is shared and always `+
      `on, so a pull request cannot have one of its own, and a workspace is `+
      `shared state, so one run's cleanup is another run's broken fixture. `+
      `The usual outcome is that the Fabric-specific parts (the deployment `+
      `pipeline, the OneLake writes, the notebook that only fails on real `+
      `data) are the parts nobody tests until they break in someone else's `+
      `run.\n\n` +
      `This puts the whole tenant on a laptop: up in seconds, offline, torn `+
      `down and recreated per test. What it cannot do is *assert* that it `+
      `behaves like Fabric, which is why every supported claim below names `+
      `the test that witnesses it, and the known differences are `+
      `[written down](37-runtime-fidelity-gaps.md) rather than left to be `+
      `discovered.\n\n` +
      `## Start here\n\n` +
      `- [Quickstart](01-quickstart.md) — compose up the family, mint a token, create a workspace, write to OneLake\n` +
      `- [Installation](02-installation.md) — brew, winget, \`go install\`, Docker, compose\n` +
      `- [Tutorial](28-tutorial-end-to-end.md) — a medallion pipeline end to end, bronze through gold\n` +
      `- [Running modes](27-running-modes.md) — default stack, lite, JVM overlay, profiles, optional DAX oracle\n` +
      `- [Architecture](03-architecture.md) — the emulator family, token acceptance, the LRO engine\n\n` +
      `## What it runs\n\n` +
      `- [Real compute](14-real-compute.md) — what actually executes, and what is still only a contract\n` +
      `- [Notebooks and Spark](20-lakesail-engine.md) — Sail as the default engine, JVM Spark as an optional oracle\n` +
      `- [Warehouse and T-SQL](16-warehouse-tds.md) — a real TDS endpoint over SQL Server, and its [T-SQL parity](29-tsql-parity.md)\n` +
      `- [OneLake](08-onelake.md) — the ADLS and Blob surfaces, shortcuts, Delta, and [OneLake security](54-onelake-security.md)\n` +
      `- [Control-plane API](07-control-plane-api.md) — every emulated endpoint, including Fabric Core MCP\n` +
      `- [Real-Time Intelligence](25-rti-kusto.md) and [Eventstream](51-eventstream-kafka.md) — KQL eventhouses and a Kafka broker\n` +
      `- [Deployment pipelines](23-deployment-pipelines.md) and [git integration](11-testing-with-fabric-cicd.md) — CI/CD against a local tenant\n\n` +
      `## How the claims are checked\n\n` +
      `Parity here is not self-assessed. Every supported capability names the ` +
      `test that witnesses it, CI fails if one loses its witness, and the ` +
      `strongest witnesses are third-party clients rather than our own tests.\n\n` +
      `- [Parity map](parity.md) — every claim, graded, with the witness that holds it\n` +
      `- [Ecosystem conformance](38-framework-conformance.md) — real vendor and OSS clients driving the emulator in CI\n` +
      `- [Runtime fidelity gaps](37-runtime-fidelity-gaps.md) — where it still differs from Fabric, stated rather than discovered\n` +
      `- [Testing](10-testing.md) — freeze the clock, inject faults, force LRO outcomes\n` +
      `- [Parity history](parity-history.md) — how the map moved, release by release\n\n` +
      `## Going further\n\n` +
      `- [Configuration](04-configuration.md) and [TLS and hosts](05-tls-and-hosts.md)\n` +
      `- [Real Fabric toggle](21-real-fabric-toggle.md) — point the same code at a real tenant\n` +
      `- [OpenMetadata](22-openmetadata.md) — catalog the emulated estate\n` +
      `- [Roadmap](13-roadmap.md) — phases P0–P3, R0–R5, S, and what landed after\n`,
  );
  // Synthesized here (no /docs source), so it has no "Edit this page"
  // target. This is the docs OVERVIEW, at /overview/ and first in the
  // sidebar; the site root is the landing page in src/pages/index.astro.
  const frontmatter =
    `---\ntitle: Overview\ndescription: A local Microsoft Fabric — control plane, OneLake, and engines that actually compute — validating real Entra tokens, with every supported claim tied to a named witness.\neditUrl: false\n---\n\n`;
  writeFileSync(join(OUT, 'overview.md'), frontmatter + body);
}

rmSync(OUT, { recursive: true, force: true });
mkdirSync(OUT, { recursive: true });
const names = readdirSync(DOCS_SRC).filter((n) => DOC_RE.test(n)).sort();
for (const name of names) {
  writeFileSync(join(OUT, name), convert(name));
}
writeOverview();
const info = writeParityHistory(OUT, PARITY, { convertBody });
// The right-sidebar picker is an Astro component and can't shell out to git, so
// hand it the same points as a build-time manifest.
const DATA = join(here, '..', 'src', 'data');
mkdirSync(DATA, { recursive: true });
writeFileSync(join(DATA, 'parity-versions.json'), JSON.stringify(parityManifest(PARITY), null, 2) + '\n');

// EVERY NUMBER ON THE LANDING PAGE IS COUNTED HERE, none is typed into the
// page. The old landing page carried "113 supported capability claims" long
// after the figure was 120 — a number in prose has no idea a row was added,
// and the page most likely to be read is the least likely to be re-read. A
// figure this file cannot compute does not go on the page.
const stats = { version: PARITY.version, latestRelease: PARITY.latestRelease,
                parity: parityStats(PARITY), docs: names.length };
// Witness kinds are not equal evidence and the page says so, so they are
// counted separately: `ci:` is a CI job driving a real third-party client,
// which is the strongest kind this repo recognises.
try {
  const witnesses = JSON.parse(readFileSync(join(REPO, 'docs', 'witnesses.json'), 'utf8'));
  // `_gated` is not a claim. It records WHY a credited witness is allowed to
  // skip, and counting it as one has been inflating the claim total by exactly
  // one for as long as this block has existed.
  const claims = Object.entries(witnesses)
    .filter(([key]) => key !== '_gated')
    .map(([, claim]) => claim);
  const all = claims.flatMap((c) => c.witnesses ?? []);
  stats.witnesses = {
    claims: claims.length,
    total: all.length,
    // References are not things: a claim is a row, a witness is a test or a
    // job, and several rows legitimately rest on the same one. `total` counts
    // the credits, `distinct` counts what actually runs.
    distinct: new Set(all).size,
    ci: all.filter((w) => w.startsWith('ci:')).length,
    jobs: new Set(all.filter((w) => w.startsWith('ci:')).map((w) => w.slice(3))).size,
  };
} catch {
  // No manifest, no numbers: the page renders an em dash rather than a guess.
}
// e2e suites: each directory under e2e/ is one real-client scenario. Counted
// rather than stated for the same reason as everything else here.
try {
  stats.e2e = readdirSync(join(REPO, 'e2e'), { withFileTypes: true })
    .filter((d) => d.isDirectory()).length;
} catch {
  /* not a full checkout */
}
writeFileSync(join(DATA, 'site-stats.json'), JSON.stringify(stats, null, 2) + '\n');

console.log(
  `sync-docs: wrote ${names.length} docs + overview to src/content/docs/ ` +
    `(parity ${info.version}; ${info.snapshots.length} tagged snapshot(s); ` +
    `${stats.parity.real} real of ${stats.parity.total} rows)`,
);
