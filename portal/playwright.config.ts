import { defineConfig, devices } from '@playwright/test';

// Headless smoke: serve the *built* dist with `vite preview` and assert the
// SPA actually mounts in a real browser. Unit tests (jsdom) and `vite build`
// can't catch a bundle that resolves Svelte to its server build and never
// mounts — this is the guard that would have.
export default defineConfig({
  testDir: './smoke',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  reporter: 'line',
  use: { baseURL: 'http://localhost:4174', trace: 'off' },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: 'vite preview --port 4174 --strictPort',
    port: 4174,
    reuseExistingServer: !process.env.CI,
  },
});
