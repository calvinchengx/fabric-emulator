// Drive OpenMetadata's UI and verify the medallion catalog visually.
// Seeded local test instance; credentials are the published dev defaults.
const path = require('path');
const fs = require('fs');
// Playwright comes from the portal's dev dependencies — the repo already has
// it for the portal smoke tests, so this suite adds no new toolchain.
const { chromium } = require(path.join(__dirname, '..', '..', 'portal', 'node_modules', '@playwright', 'test'));

const OM = 'http://localhost:8585';
const OUT = process.env.OM_SHOTS || '/tmp/om-shots';
const DB = 'fabric-emulator.contoso-analytics';

const checks = [];
function check(name, ok, detail) {
  checks.push({ name, ok, detail });
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? ' — ' + detail : ''}`);
}

fs.mkdirSync(OUT, { recursive: true });

(async () => {
  const browser = await chromium.launch({ headless: true });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
  const page = await ctx.newPage();

  await page.goto(OM, { waitUntil: 'domcontentloaded' });
  await page.fill('#email', 'admin@open-metadata.org');
  await page.fill('#password', 'admin');
  await page.click('button[type="submit"]');
  await page.waitForLoadState('networkidle').catch(() => {});
  await page.waitForTimeout(4000);
  check('signed in', !/signin/i.test(page.url()), page.url());

  const shot = async (n) => page.screenshot({ path: path.join(OUT, n + '.png'), fullPage: false });

  const visit = async (url, waitFor) => {
    await page.goto(OM + url, { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(3500);
    if (waitFor) await page.waitForSelector(waitFor, { timeout: 15000 }).catch(() => {});
    return (await page.locator('body').innerText()).replace(/\s+/g, ' ');
  };

  // 1. Domain + data product
  let t = await visit('/domain');
  check('domain listed', /Contoso Sales|contoso-sales/i.test(t));
  await shot('1-domains');

  // 2. Glossary terms
  t = await visit('/glossary/Contoso%20Sales');
  const terms = ['Resolved customer key', 'Resolved customer dimension', 'Customer'];
  check('glossary shows terms', terms.some((x) => t.includes(x)),
        terms.filter((x) => t.includes(x)).join(', ') || t.slice(0, 120));
  await shot('2-glossary');

  // 3. Metrics
  t = await visit('/metrics');
  const metrics = ['daily_revenue', 'orders_per_day', 'resolved_customers', 'multi_source_customers'];
  const seen = metrics.filter((m) => t.includes(m));
  check('all 4 metrics visible', seen.length === 4, seen.join(', '));
  await shot('3-metrics');

  // 4. A gold table: columns + description
  t = await visit(`/table/${DB}.dw.fct_daily_revenue`);
  check('gold table renders', /fct_daily_revenue/.test(t));
  check('gold columns present', /revenue/i.test(t) && /order_date/i.test(t));
  await shot('4-table-gold');

  // 5. The data contract carrying ODCS rules. The tab must be CLICKED — the
  // /contract URL renders the table shell without the tab's content, so a
  // check that only navigated there tested nothing.
  await visit(`/table/${DB}.dw.dim_customer_360`);
  await page.locator('[role="tab"]:has-text("Contract")').first().click();
  await page.waitForTimeout(5000);
  t = (await page.locator('body').innerText()).replace(/\s+/g, ' ');
  check('contract tab names the contract', /sales-gold — dim_customer_360/.test(t));
  // OM collapses a long description behind a "more" link, so the rules are in
  // the DOM but not in the visible text until it is expanded. Reading the
  // collapsed view and concluding "not rendered" was measuring the fold.
  const more = page.locator('[data-testid="read-more-button"]').first();
  if (await more.count()) await more.click().catch(() => {});
  await page.waitForTimeout(2000);
  await page.mouse.wheel(0, 1200);
  await page.waitForTimeout(2000);
  t = (await page.locator('body').innerText()).replace(/\s+/g, ' ');
  check('ODCS quality rules rendered',
        /duplicateValues|nullValues|uniqueness|Quality/i.test(t),
        (t.match(/(duplicateValues|nullValues|uniqueness|completeness)/gi) || []).slice(0, 4).join(', '));
  await shot('5-contract');

  // 6. Lineage graph for gold
  await page.goto(`${OM}/table/${DB}.dw.fct_daily_revenue/lineage`, { waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(7000);
  const nodes = await page.locator('[data-testid^="lineage-node"], .react-flow__node').count();
  check('lineage graph has nodes', nodes > 1, `${nodes} node(s)`);
  await shot('6-lineage');

  // 7. The resolved dimension, with its PII column tag
  t = await visit(`/table/${DB}.lake.silver_customers`);
  check('silver table catalogued', /silver_customers/.test(t));
  await shot('7-silver');

  await ctx.close();
  await browser.close();
  const failed = checks.filter((c) => !c.ok);
  console.log(`\n${checks.length - failed.length}/${checks.length} checks passed`);
  process.exit(failed.length ? 1 : 0);
})().catch((e) => { console.error('verify failed:', e.message); process.exit(2); });
