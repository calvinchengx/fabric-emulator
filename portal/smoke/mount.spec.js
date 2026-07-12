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
  await expect(page.getByRole('link', { name: 'Workspace identities' })).toBeVisible();
  await expect(page.getByText('Fabric Emulator').first()).toBeVisible();

  // A failed mount throws an uncaught Svelte error; API calls are caught, so
  // there should be no uncaught exceptions.
  expect(jsErrors, jsErrors.join('\n')).toEqual([]);
});
