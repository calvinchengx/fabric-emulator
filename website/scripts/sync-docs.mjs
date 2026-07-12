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

const here = dirname(fileURLToPath(import.meta.url));
const DOCS_SRC = join(here, '..', '..', 'docs');
const OUT = join(here, '..', 'src', 'content', 'docs');
export const BASE = '/fabric-emulator/';

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

function convert(name) {
  const raw = readFileSync(join(DOCS_SRC, name), 'utf8');
  const lines = raw.split('\n');
  const h1Index = lines.findIndex((l) => /^#\s+/.test(l));
  const title = h1Index >= 0 ? cleanTitle(lines[h1Index].replace(/^#\s+/, '')) : name.replace(/\.md$/, '');
  // Drop the H1 (Starlight renders the frontmatter title) and a trailing blank.
  if (h1Index >= 0) {
    lines.splice(h1Index, lines[h1Index + 1]?.trim() === '' ? 2 : 1);
  }
  const body = rewriteLinks(lines.join('\n').replace(/^\n+/, ''));
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
      `- [Architecture](01-architecture.md) — the two-emulator model, token acceptance, the LRO engine\n` +
      `- [API surface](02-api-surface.md) — every emulated endpoint and wire shape\n` +
      `- [Roadmap](03-roadmap.md) — phases P0–P3 and what's shipped\n`,
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
console.log(`sync-docs: wrote ${names.length} docs + index to src/content/docs/`);
