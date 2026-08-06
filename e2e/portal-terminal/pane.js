// e2e: the Flow view's terminal pane, driven in a real browser.
//
// The pane is a real feature with a real attack surface — a shell reachable
// through the portal's own origin — and until this existed it was witnessed by
// nothing but unit tests against an httptest server. Those cover the proxy's
// rules; none of them can say that a browser, an iframe, a websocket upgrade
// through the emulator, and ttyd in another container actually add up to a
// working terminal.
//
// WHAT THIS PROVES, and the bug behind each claim:
//
//  1. The toggle appears at all. The status endpoint DIALS ttyd rather than
//     trusting configuration, so the toggle is proof the emulator reached the
//     container. This is the spark-agent bug in miniature: FABRIC_SPARK_AGENT_URL
//     was set in a base compose file while the service sat behind a profile
//     nobody enabled, and every PySpark leg failed naming nothing. Here the URL
//     comes from docker-compose.terminal.yml and the service from
//     `--profile terminal`, and this asserts that documented pairing works.
//
//  2. The pane attaches. Read from xterm's BUFFER, never the DOM: xterm.js
//     renders to <canvas>, so a DOM query matches nothing and a pixel sample
//     reads back one flat colour. A sibling recorder reported a false negative
//     on a recording where the pane was working perfectly.
//
//  3. A typed command RUNS. `ttyd -W` is what makes the client writable, and
//     without it the pane is a read-only viewer — a different product that
//     would pass claims 1 and 2 unchanged.
//
//  4. A wrong token is refused. The portal has no auth at all; this one route
//     carries its own, and it is the only thing between anyone who can reach
//     9443 and a shell.
//
//  5. A plain portal request still answers while the pane is live. The proxy
//     once hijacked EVERY request rather than only genuine websocket upgrades,
//     splicing the browser's keep-alive connection to ttyd — so
//     `GET /_emulator/portal/lineage` came back as ttyd's HTML 404. Diagnosed
//     from the first 120 bytes of a response body during a recording.
//
// Playwright comes from the portal's dev dependencies, as e2e/medallion-governance
// does: the repo already has it for the portal smoke tests, so this adds no
// toolchain.
const path = require('path');
const { chromium } = require(path.join(__dirname, '..', '..', 'portal', 'node_modules', '@playwright', 'test'));

const PORTAL = process.env.TERM_PORTAL_URL || 'https://localhost:9443';
const TOKEN = process.env.TERM_TOKEN || 'e2e-terminal-token';
const MARKER = 'fabric-emulator-pane-ok';

const checks = [];
function check(name, ok, detail) {
  checks.push({ name, ok, detail });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? ' — ' + detail : ''}`);
}

/** Every non-empty line xterm currently holds, as one string.
 *
 * ttyd 1.7 exposes its Terminal as `window.term`; the buffer is the only
 * surface that says what the terminal SHOWS (see the header).
 */
async function bufferText(frame) {
  return frame
    .evaluate(() => {
      const buf = window.term?.buffer?.active;
      if (!buf) return '';
      const out = [];
      for (let i = 0; i < buf.length; i++) {
        const s = buf.getLine(i)?.translateToString(true)?.trim();
        if (s) out.push(s);
      }
      return out.join('\n');
    })
    .catch(() => '');
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** Poll until `read()` satisfies `ok`, or give up and return the last value. */
async function until(read, ok, timeoutMs = 60000, everyMs = 500) {
  const end = Date.now() + timeoutMs;
  let last = await read();
  while (Date.now() < end) {
    if (ok(last)) return last;
    await sleep(everyMs);
    last = await read();
  }
  return last;
}

(async () => {
  const browser = await chromium.launch({ headless: true });
  const ctx = await browser.newContext({
    viewport: { width: 1600, height: 900 },
    // Self-signed, as the whole stack is.
    ignoreHTTPSErrors: true,
  });
  const page = await ctx.newPage();

  await page.goto(`${PORTAL}/#flow`, { waitUntil: 'domcontentloaded' });

  // 1. The toggle exists — i.e. the emulator dialled ttyd and got an answer.
  const toggle = page.getByRole('button', { name: 'Terminal' });
  let reachable = true;
  try {
    await toggle.waitFor({ state: 'visible', timeout: 60000 });
  } catch {
    reachable = false;
  }
  const why = reachable
    ? ''
    : (await page.locator('text=/Terminal unavailable/').textContent().catch(() => null)) ||
      'no toggle and no reason shown';
  check('emulator reached ttyd through the compose network', reachable, why);
  if (!reachable) return finish(browser);

  // 4. …before connecting: a wrong token must be refused. Done through the
  // page's own fetch so it travels the same origin the pane would.
  const refused = await page.evaluate(async (marker) => {
    const r = await fetch(`/_emulator/portal/terminal/?token=${marker}`, { redirect: 'manual' });
    return r.status;
  }, 'not-the-token');
  check('a wrong token is refused', refused === 401 || refused === 403, `HTTP ${refused}`);

  // 2. Open the pane and let it attach.
  await toggle.click();
  await page.getByLabel('terminal token').fill(TOKEN);
  await page.getByRole('button', { name: 'Connect' }).click();

  const frameFor = () =>
    page.frames().find((f) => f.url().includes('/_emulator/portal/terminal/'));

  const frame = await until(() => frameFor(), (f) => !!f, 30000);
  check('the pane loaded ttyd from the portal origin', !!frame, frame ? frame.url() : 'no frame');
  if (!frame) return finish(browser);

  const attached = await until(() => bufferText(frame), (t) => t.length > 0);
  check('the terminal attached', attached.length > 0,
        `${attached.split('\n').length} line(s)`);
  if (!attached) return finish(browser);

  // 3. A typed command actually runs. The marker is echoed by the shell, so
  // finding it TWICE (the typed line and its output) would be ambiguous —
  // build it so the output line is distinguishable from the command.
  await frame.click('body').catch(() => {});
  await page.keyboard.type(`echo ${MARKER}-$((6*7))`);
  await page.keyboard.press('Enter');
  const ran = await until(
    () => bufferText(frame),
    (t) => t.includes(`${MARKER}-42`),
    45000,
  );
  check('a command typed in the pane ran a real shell', ran.includes(`${MARKER}-42`),
        ran.includes(`${MARKER}-42`) ? 'shell evaluated $((6*7))' : lastLines(ran));

  // 5. The proxy must not have captured the browser's connection. A plain
  // portal route, while the pane is live, has to come back as the portal's own
  // JSON — not ttyd's HTML.
  const plain = await page.evaluate(async () => {
    const r = await fetch('/_emulator/portal/lineage');
    const body = await r.text();
    return { status: r.status, head: body.slice(0, 120) };
  });
  const isPortalJson = plain.status === 200 && plain.head.trimStart().startsWith('{');
  check('a plain portal request still answers while the pane is live', isPortalJson,
        `HTTP ${plain.status} body=${JSON.stringify(plain.head)}`);

  return finish(browser);
})().catch((e) => {
  console.error(e);
  process.exit(2);
});

function lastLines(t, n = 6) {
  return JSON.stringify(t.split('\n').slice(-n).join(' | ').slice(0, 300));
}

async function finish(browser) {
  await browser.close();
  const failed = checks.filter((c) => !c.ok);
  console.log(`\n${checks.length - failed.length}/${checks.length} checks passed`);
  if (failed.length) {
    console.error('FAILED: ' + failed.map((c) => c.name).join('; '));
    process.exit(1);
  }
  process.exit(0);
}
