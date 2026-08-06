import { test, expect } from '@playwright/test';

// The regression this guards: a build where Svelte resolves to its server
// bundle mounts nothing (#app stays empty), yet `vite build` and the jsdom unit
// tests still pass. Only a real browser catches it.
test('the portal mounts and renders its shell', async ({ page }) => {
  const jsErrors = [];
  page.on('pageerror', (e) => jsErrors.push(e.message));

  await page.goto('/');

  // #app must actually receive the mounted component (the thing that broke).
  await expect(page.locator('#app')).not.toBeEmpty();
  // Concrete shell chrome renders (sidebar nav + topbar), not just a stray node.
  await expect(page.getByRole('link', { name: 'Workspaces' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Connections' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Capacities' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Jobs' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'OneLake shortcuts' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Warehouse SQL' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Workspace identities' })).toBeVisible();
  await expect(page.getByText('Fabric Emulator').first()).toBeVisible();

  // A failed mount throws an uncaught Svelte error; API calls are caught, so
  // there should be no uncaught exceptions.
  expect(jsErrors, jsErrors.join('\n')).toEqual([]);
});

// The first paint belongs to index.html's inline script, which runs BEFORE the
// bundle so the portal never flashes the wrong theme. jsdom does not run it, so
// the unit tests cannot see this half at all — and that script duplicates the
// resolution logic in src/theme.ts, which is exactly the kind of pair that
// drifts silently.
test('a fresh visitor gets dark, painted before the bundle loads', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'light' }); // the machine says light…
  await page.goto('/');

  // …and the portal is dark anyway: dark is the default, not a reflection of
  // the machine.
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
  expect(await page.evaluate(() => document.documentElement.style.colorScheme)).toBe('dark');
  await expect(page.getByRole('button', { name: /Theme: dark/ })).toBeVisible();
});

test('an explicit "follow the system" choice survives and is obeyed', async ({ page }) => {
  // Stored as a word rather than as the absence of a key: absence now means
  // dark, so clearing would quietly demote this back to the default.
  await page.addInitScript(() => localStorage.setItem('fe.theme', 'system'));
  await page.emulateMedia({ colorScheme: 'light' });
  await page.goto('/');
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
});
