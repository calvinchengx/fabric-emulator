// Film the Data flow view while a medallion builds behind it.
//
// The companion of demo.tape: that one records a terminal with VHS, this one
// records a browser with Playwright, and both are the source of truth for the
// GIF they produce. flow.gif used to be a hand-cropped excerpt of a run in a
// DIFFERENT repository, which meant nobody could regenerate the README's hero
// image — and it had gone stale enough to predate the terminal pane.
//
// RECORDED AT GIF DIMENSIONS, not cropped afterwards. The old asset was
// 760x700: a crop of a 1600x900 recording, so its framing depended on where the
// portal's layout happened to put things that week. Recording at the size the
// GIF ships in makes the framing a property of this file.
//
// ORDER MATTERS. flow.py starts this recorder, waits for `.rolling`, and only
// then seeds — so the graph is empty when filming starts and fills in on
// camera. A recorder that attached afterwards would film a finished graph,
// which says nothing about watching data move.
const fs = require('fs');
const path = require('path');
const { chromium } = require(path.join(__dirname, '..', '..', 'portal', 'node_modules', '@playwright', 'test'));

const OUT = process.env.FLOW_OUT || '/tmp/flow-capture';
const PORTAL = process.env.FLOW_PORTAL || 'https://localhost:9443';
const W = Number(process.env.FLOW_WIDTH || 900);
const H = Number(process.env.FLOW_HEIGHT || 620);
const MAX_SECONDS = Number(process.env.FLOW_MAX_SECONDS || 180);
const ROLLING = path.join(OUT, '.rolling');
const STOP = path.join(OUT, '.stop');

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  for (const f of [ROLLING, STOP]) if (fs.existsSync(f)) fs.unlinkSync(f);

  const browser = await chromium.launch();
  const ctx = await browser.newContext({
    viewport: { width: W, height: H },
    // Self-signed, as the whole local stack is.
    ignoreHTTPSErrors: true,
    recordVideo: { dir: OUT, size: { width: W, height: H } },
  });
  const page = await ctx.newPage();

  await page.goto(`${PORTAL}/#flow`, { waitUntil: 'domcontentloaded' });

  // Fold the navigation: 240px of sidebar in a 900px frame is a quarter of the
  // image spent on links nobody can click in a GIF.
  await page
    .getByRole('button', { name: 'Toggle sidebar' })
    .click()
    .catch(() => console.error('no sidebar toggle — portal predates the split layout'));

  // Wait for the stream to say it is connected, so the first event cannot land
  // before the log is listening.
  await page.waitForFunction(
    () => !!document.body.textContent?.includes('streaming'),
    { timeout: 30000 },
  ).catch(() => console.error('the SSE chip never said streaming'));

  fs.writeFileSync(ROLLING, String(process.pid));
  console.log('ROLLING');

  // Sample while the seeding runs, so the log can say what was actually filmed.
  let maxNodes = 0;
  let maxRows = 0;
  const deadline = Date.now() + MAX_SECONDS * 1000;
  while (Date.now() < deadline && !fs.existsSync(STOP)) {
    await page.waitForTimeout(500);
    const nodes = await page.locator('svg g.node, svg a.node').count().catch(() => 0);
    const rows = await page.locator('table tbody tr').count().catch(() => 0);
    if (nodes > maxNodes) maxNodes = nodes;
    if (rows > maxRows) maxRows = rows;
  }

  // Hold on the finished graph for a beat. A GIF that cuts the instant the last
  // edge lands gives a reader no time to see the shape it drew.
  await page.waitForTimeout(2500);

  await page.screenshot({ path: path.join(OUT, 'flow-final.png') });

  // Claim the video by name BEFORE the context closes its handle: Playwright
  // names them randomly, so scanning the directory afterwards can pick up a
  // previous run's file — which once made a check pass on a recording that
  // never happened.
  const dest = path.join(OUT, 'flow.webm');
  const video = page.video();
  await ctx.close(); // flushes the video; must precede saveAs
  let saved = false;
  if (video) {
    await video.saveAs(dest);
    await video.delete();
    saved = fs.existsSync(dest);
  }
  await browser.close();

  console.log(`NODES ${maxNodes}`);
  console.log(`ROWS ${maxRows}`);
  console.log(`VIDEO ${saved ? dest : 'none'}`);

  // A blank recording is worse than none: it ships as the README's hero image
  // and looks like the product does nothing.
  if (!saved || maxNodes < 2 || maxRows < 1) {
    console.error(
      `refusing this take: nodes=${maxNodes} (need 2+), log rows=${maxRows} (need 1+), video=${saved}`,
    );
    process.exit(1);
  }
  process.exit(0);
})().catch((e) => {
  console.error(e);
  process.exit(2);
});
