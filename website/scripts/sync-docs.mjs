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

// NO writeOverview() ANY MORE, and this note is here so its absence reads as a
// decision rather than an omission.
//
// The docs site had two homes. `/docs/` is src/pages/index.astro and `/docs/
// overview/` was synthesized here, and the two said the same thing in two
// formats: 81 shared ten-word runs, a sidebar whose first entry sent a reader
// straight from one to the other, and no single place to edit either. The
// overview's own contribution was a curated index, so those links moved onto
// the docs root rather than being deleted with it.
//
// `/overview/` still resolves — astro.config.mjs redirects it and
// assemble_site.py keeps the site-root stub, because published-routes.txt
// records that the route was once served and the assembler fails if a
// published route would 404.

rmSync(OUT, { recursive: true, force: true });
mkdirSync(OUT, { recursive: true });
const names = readdirSync(DOCS_SRC).filter((n) => DOC_RE.test(n)).sort();
for (const name of names) {
  writeFileSync(join(OUT, name), convert(name));
}
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
  `sync-docs: wrote ${names.length} docs to src/content/docs/ ` +
    `(parity ${info.version}; ${info.snapshots.length} tagged snapshot(s); ` +
    `${stats.parity.real} real of ${stats.parity.total} rows)`,
);
