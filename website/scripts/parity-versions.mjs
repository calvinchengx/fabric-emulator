// Parity-map versioning, driven entirely by git tags (no frozen copies to
// maintain). The canonical parity map lives in /docs; each `v*` release tag is
// a snapshot git already holds. This module, called from sync-docs.mjs:
//
//   - resolves the version string from `git describe --tags`;
//   - for every `v*` tag that contains a parity file, emits a read-only
//     snapshot page under parity-history/<version>/;
//   - generates a changelog by diffing the parity tables between consecutive
//     versions (which rows changed status, were added, or removed);
//   - writes a parity-history index linking the live map + every snapshot.
//
// Note: the parity map was introduced after v0.1.0, so until a release ships
// that includes it, the only "version" is the unreleased tip. Everything below
// degrades gracefully to that single point.
import { execSync } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const PARITY_RE = /-parity\.md$/;

function git(repo, args) {
  return execSync(`git ${args}`, { cwd: repo, stdio: ['ignore', 'pipe', 'ignore'] }).toString();
}

// The version string a build reflects, e.g. "v0.2.0" on a tag or
// "v0.1.0-69-g1935665" between releases. Falls back to a short sha, then "dev".
export function gitVersion(repo) {
  for (const cmd of ['describe --tags --always', 'rev-parse --short HEAD']) {
    try {
      const v = git(repo, cmd).trim();
      if (v) return v;
    } catch {
      /* not a git checkout / no tags — try the next */
    }
  }
  return 'dev';
}

// True for "v1.2.3" style strings straight off a tag (no -N-g<sha> suffix).
const isRelease = (v) => /^v\d+\.\d+\.\d+$/.test(v);

function parityPathAt(repo, ref) {
  try {
    return git(repo, `ls-tree --name-only ${ref} docs/`).split('\n').find((n) => PARITY_RE.test(n)) || null;
  } catch {
    return null;
  }
}

function releaseTags(repo) {
  try {
    return git(repo, 'tag --list v* --sort=v:refname').split('\n').filter(Boolean);
  } catch {
    return [];
  }
}

// Parse every 3+-column markdown table row into feature -> status, keyed by the
// first cell. The status is the leading emoji of the last cell (🟢/🟡/🟠/🔴).
// The doc has several tables with different headers ("Fabric feature", "Real
// client", …), so headers are detected structurally: a row whose next line is
// the `|---|` separator is a header and skipped, along with the separator.
const isSeparatorRow = (l) => /^\s*\|[\s:|-]+\|\s*$/.test(l || '') && l.includes('-');

function parseParity(md) {
  const map = new Map();
  const lines = md.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (!/^\s*\|/.test(line)) continue;
    if (isSeparatorRow(line)) continue; // the |---| rule
    if (isSeparatorRow(lines[i + 1])) continue; // header row (rule follows it)
    const cells = line.split('|').slice(1, -1).map((c) => c.trim());
    if (cells.length < 3) continue;
    const feature = cells[0].replace(/[*`]/g, '').trim();
    if (!feature) continue;
    const status = cells[cells.length - 1];
    const emoji = (status.match(/🟢|🟡|🟠|🔴/) || [''])[0];
    map.set(feature, { emoji, status });
  }
  return map;
}

function statusTally(map) {
  const t = { '🟢': 0, '🟡': 0, '🟠': 0, '🔴': 0 };
  for (const { emoji } of map.values()) if (t[emoji] !== undefined) t[emoji]++;
  return t;
}

function tallyLine(t) {
  return `${t['🟢']} 🟢 Real · ${t['🟡']} 🟡 Emulated · ${t['🟠']} 🟠 BYO-engine · ${t['🔴']} 🔴 Not implemented`;
}

function diffParity(prev, cur) {
  const added = [];
  const removed = [];
  const changed = [];
  for (const [f, v] of cur) {
    if (!prev.has(f)) added.push({ f, to: v.emoji });
    else if (prev.get(f).emoji !== v.emoji) changed.push({ f, from: prev.get(f).emoji, to: v.emoji });
  }
  for (const f of prev.keys()) if (!cur.has(f)) removed.push({ f });
  return { added, removed, changed };
}

// versionSlug("v0.1.0-69-g1935665") -> "v0-1-0-69-g1935665" (route-safe).
const versionSlug = (v) => v.replace(/[.+]/g, '-');

/**
 * Emit parity-history/ pages. `helpers` provides { convertBody, editUrlFor }
 * from sync-docs so snapshots share the same H1-stripping + link-rewriting.
 * Returns the resolved version string so the caller can stamp the live map.
 */
export function writeParityHistory(OUT, repo, helpers) {
  const version = gitVersion(repo);
  const outDir = join(OUT, 'parity-history');
  mkdirSync(outDir, { recursive: true });

  // Ordered points, oldest -> newest: each release tag that carries a parity
  // file, then the working-tree "latest" (which may be unreleased).
  const points = [];
  for (const tag of releaseTags(repo)) {
    const p = parityPathAt(repo, tag);
    if (p) points.push({ label: tag, released: true, md: git(repo, `show ${tag}:${p}`) });
  }
  const livePath = parityPathAt(repo, 'HEAD');
  const liveMd = livePath ? git(repo, `show HEAD:${livePath}`) : '';
  const liveSlug = livePath ? livePath.replace(/^docs\//, '').replace(/\.md$/, '') : null;
  points.push({ label: version, released: isRelease(version), latest: true, md: liveMd });

  // Snapshot page per released tag (the "latest" point links to the live map
  // instead of duplicating it).
  for (const pt of points) {
    if (pt.latest) continue;
    const slug = versionSlug(pt.label);
    const body = helpers.convertBody(pt.md);
    const fm = `---\ntitle: ${JSON.stringify(`Parity — ${pt.label}`)}\neditUrl: false\nprev: false\nnext: false\n---\n\n`;
    const banner = `:::note[Historical snapshot]\nThe feature-parity map as of release **${pt.label}**. The current map is on the [Parity page](/fabric-emulator/${liveSlug}/).\n:::\n\n`;
    writeFileSync(join(outDir, `${slug}.md`), fm + banner + body);
  }

  // Changelog: diff consecutive points.
  const cl = [];
  for (let i = 1; i < points.length; i++) {
    const a = parseParity(points[i - 1].md);
    const b = parseParity(points[i].md);
    const { added, removed, changed } = diffParity(a, b);
    const to = points[i].label + (points[i].latest && !points[i].released ? ' (unreleased)' : '');
    cl.push(`## ${points[i - 1].label} → ${to}\n`);
    if (!added.length && !removed.length && !changed.length) {
      cl.push('_No parity changes._\n');
      continue;
    }
    for (const c of changed) cl.push(`- **${c.f}**: ${c.from || '—'} → ${c.to || '—'}`);
    for (const a2 of added) cl.push(`- **${a2.f}**: added ${a2.to || ''}`.trim());
    for (const r of removed) cl.push(`- **${r.f}**: removed`);
    cl.push('');
  }

  const liveTally = liveMd ? tallyLine(statusTally(parseParity(liveMd))) : '';
  const releasedPoints = points.filter((p) => p.released && !p.latest);

  const clFm = `---\ntitle: Parity changelog\neditUrl: false\n---\n\n`;
  const clBody =
    `How the [feature-parity map](/fabric-emulator/${liveSlug}/) changed across releases — ` +
    `generated by diffing the parity tables between consecutive \`v*\` tags.\n\n` +
    (liveTally ? `**Current (${version}):** ${liveTally}.\n\n` : '') +
    (cl.length
      ? cl.join('\n')
      : `_No tagged release includes the parity map yet — the map was introduced after ${releaseTags(repo)[0] ?? 'the first tag'}. ` +
        `The first entry here appears when a release ships that carries it._\n`);
  writeFileSync(join(outDir, 'changelog.md'), clFm + clBody);

  // Index: the live map + every snapshot.
  const idxFm = `---\ntitle: Parity history\neditUrl: false\n---\n\n`;
  const rows = [
    `- **[Current — ${version}${isRelease(version) ? '' : ' (unreleased)'}](/fabric-emulator/${liveSlug}/)** — the live map on \`main\``,
    ...releasedPoints
      .slice()
      .reverse()
      .map((p) => `- [${p.label}](/fabric-emulator/parity-history/${versionSlug(p.label)}/) — snapshot at release`),
  ];
  const idxBody =
    `Versions of the [feature-parity map](/fabric-emulator/${liveSlug}/), tracked by git release tags. ` +
    `See the [parity changelog](/fabric-emulator/parity-history/changelog/) for what changed between them.\n\n` +
    rows.join('\n') +
    '\n\n' +
    (releasedPoints.length
      ? ''
      : `:::note\nOnly the unreleased tip carries a parity map so far (it was added after \`${releaseTags(repo)[0] ?? 'v0.1.0'}\`). ` +
        `Each future \`vX.Y.Z\` release will appear above automatically.\n:::\n`);
  writeFileSync(join(outDir, 'index.md'), idxFm + idxBody);

  return { version, snapshots: releasedPoints.map((p) => p.label), liveSlug };
}
