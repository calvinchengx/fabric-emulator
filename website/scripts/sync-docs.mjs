// Generates Starlight content from the canonical Markdown in /docs, keeping
// /docs as the single source of truth (its files stay pristine and their
// GitHub-relative links keep working). Run automatically before dev/build.
//
// For each docs/NN-name.md it: derives the title from the leading H1, injects
// Starlight frontmatter, drops the duplicate H1, and rewrites intra-doc
// `NN-name.md` links to site routes under the configured base.
import { readdirSync, readFileSync, writeFileSync, rmSync, mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { collectParity, writeParityHistory, parityManifest } from './parity-versions.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const REPO = join(here, '..', '..');
const DOCS_SRC = join(REPO, 'docs');
const OUT = join(here, '..', 'src', 'content', 'docs');
export const BASE = '/fabric-emulator/';

// Parity version data (release tags + the live map), collected once. `version`
// is e.g. "v0.2.0" on a tag or "v0.1.0-69-g1935665" between releases.
const PARITY = collectParity(REPO);
const IS_RELEASE = /^v\d+\.\d+\.\d+$/.test(PARITY.version);
const PARITY_RE = /-parity\.md$/;

// Rewrite `](./|docs/ NN-slug.md#anchor)` → `](/fabric-emulator/NN-slug/#anchor)`.
const LINK_RE = /\]\((?:\.\/|docs\/)?(\d{2}-[a-z0-9-]+)\.md(#[^)]*)?\)/g;
function rewriteLinks(md) {
  return md.replace(LINK_RE, (_m, slug, anchor) => `](${BASE}${slug}/${anchor ?? ''})`);
}

// "02 — Emulated API surface" → "Emulated API surface".
function cleanTitle(h1) {
  return h1.replace(/^\d+[a-z]?\s*[—:-]\s*/i, '').trim();
}

function yamlEscape(s) {
  return '"' + s.replace(/"/g, '\\"') + '"';
}

// Strip the leading H1 (Starlight renders the frontmatter title) and rewrite
// intra-doc links. Shared with the parity snapshot generator so historical
// snapshots convert identically.
function convertBody(raw) {
  const lines = raw.split('\n');
  const h1Index = lines.findIndex((l) => /^#\s+/.test(l));
  if (h1Index >= 0) {
    lines.splice(h1Index, lines[h1Index + 1]?.trim() === '' ? 2 : 1);
  }
  return rewriteLinks(lines.join('\n').replace(/^\n+/, ''));
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
  let body = convertBody(raw);
  if (PARITY_RE.test(name)) body = parityStamp() + body;
  // Point "Edit this page" at the real source in /docs (the generated copy
  // under src/content/docs/ is git-ignored), not Starlight's default path.
  const editUrl = `https://github.com/calvinchengx/fabric-emulator/edit/main/docs/${name}`;
  const frontmatter = `---\ntitle: ${yamlEscape(title)}\neditUrl: ${yamlEscape(editUrl)}\n---\n\n`;
  return frontmatter + body;
}

function writeIndex() {
  const body = rewriteLinks(
    `Local emulator of the **Microsoft Fabric control plane** in a single Go binary — ` +
      `workspaces, items and their CI/CD definitions, workspace RBAC, git integration, jobs, and ` +
      `the 202/poll long-running-operation contract, all validating Microsoft Entra bearer tokens. ` +
      `Pairs with its sibling [entra-emulator](https://calvinchengx.github.io/entra-emulator/) — ` +
      `fabric-emulator validates tokens against its JWKS exactly as real Fabric validates against ` +
      `Entra — so you can test service-principal automation and \`fabric-cicd\` pipelines offline ` +
      `with no capacity and no cloud tenant.\n\n` +
      `:::caution\nLocal development tool only — intentionally insecure (no real authorization ` +
      `boundary, self-signed TLS). It emulates the control-plane **contract**, not the runtime: ` +
      `nothing actually computes. Run it on \`localhost\` only.\n:::\n\n` +
      `## Start here\n\n` +
      `- [Quickstart](01-quickstart.md) — compose up the pair, mint a token, create a workspace, write to OneLake\n` +
      `- [Installation](02-installation.md) — brew, winget, go install, Docker, compose\n` +
      `- [Architecture](03-architecture.md) — the two-emulator model, token acceptance, the LRO engine\n` +
      `- [Control-plane API](07-control-plane-api.md) and [OneLake](08-onelake.md) — every emulated endpoint\n` +
      `- [Testing](10-testing.md) — freeze the clock, inject faults; [run the real fabric-cicd](11-testing-with-fabric-cicd.md)\n` +
      `- [Roadmap](13-roadmap.md) — phases P0–P3 and what's next\n`,
  );
  // The landing page is synthesized here (no /docs source), so it has no
  // "Edit this page" target.
  const frontmatter =
    `---\ntitle: Fabric Emulator\ndescription: A local emulator of the Microsoft Fabric control plane that validates Entra bearer tokens.\neditUrl: false\n---\n\n`;
  writeFileSync(join(OUT, 'index.md'), frontmatter + body);
}

rmSync(OUT, { recursive: true, force: true });
mkdirSync(OUT, { recursive: true });
const names = readdirSync(DOCS_SRC).filter((n) => /^\d{2}-.*\.md$/.test(n)).sort();
for (const name of names) {
  writeFileSync(join(OUT, name), convert(name));
}
writeIndex();
const info = writeParityHistory(OUT, PARITY, { convertBody });
// The right-sidebar picker is an Astro component and can't shell out to git, so
// hand it the same points as a build-time manifest.
const DATA = join(here, '..', 'src', 'data');
mkdirSync(DATA, { recursive: true });
writeFileSync(join(DATA, 'parity-versions.json'), JSON.stringify(parityManifest(PARITY), null, 2) + '\n');
console.log(
  `sync-docs: wrote ${names.length} docs + index to src/content/docs/ ` +
    `(parity ${info.version}; ${info.snapshots.length} tagged snapshot(s))`,
);
