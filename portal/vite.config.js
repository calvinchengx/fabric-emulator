import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import tailwindcss from '@tailwindcss/vite';
import path from 'node:path';

export default defineConfig(({ mode }) => ({
  plugins: [tailwindcss(), svelte()],
  base: './',
  build: { outDir: 'dist', emptyOutDir: true },
  server: {
    // Dev loop: proxy the portal API to a running fabric-emulator.
    proxy: {
      '/_emulator': { target: 'https://localhost:9443', secure: false },
      '/health': { target: 'https://localhost:9443', secure: false },
    },
  },
  // Only under Vitest, resolve Svelte's client (browser) build so components
  // mount in jsdom. In dev/build we must NOT override resolve.conditions — an
  // empty list clobbers Vite's defaults and makes Svelte resolve to its server
  // build (`mount(...)` unavailable → the app never mounts in the browser).
  resolve: {
    // $lib is what shadcn-svelte's components import each other by; this is a
    // plain Vite app rather than SvelteKit, so the alias is declared here.
    alias: { $lib: path.resolve('./src/lib') },
    ...(mode === 'test' ? { conditions: ['browser'] } : {}),
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./vitest-setup.js'],
    include: ['src/**/*.test.{js,ts}'],
    // The portal was the one tier with NO coverage measurement at all: Go has a
    // 90% floor and Python a 70% one, while the client — which is what a user
    // actually looks at — had never been measured. First reading was 86.4% of
    // our own statements, with two components (App, Identities) at zero.
    coverage: {
      provider: 'v8',
      // `all` so a file with no test at all counts as 0% rather than vanishing.
      // Without it the number flatters itself: the two 0% components would
      // simply not have appeared.
      all: true,
      include: ['src/**'],
      exclude: [
        // VENDORED. shadcn-svelte generates these into the tree; they are
        // upstream's code, they carry no portal logic, and a test asserting
        // that `<Card>` renders a div would pin someone else's implementation
        // detail. Excluded rather than omitted silently — the reason is here.
        'src/lib/**',
        // The mount call itself. Exercised by portal/smoke (a real browser
        // asserting the built bundle renders), which is where a broken
        // entrypoint is actually caught; jsdom cannot witness it.
        'src/main.js',
        // Types and styles, which carry no statements to cover.
        'src/**/*.d.ts',
        'src/app.css',
      ],
      reporter: ['text', 'json-summary'],
      thresholds: {
        // Ratcheted deliberately, not aspirationally: these are the numbers the
        // suite actually holds, so a regression fails the build the day it
        // lands. Raise them as gaps close; never lower them to make CI pass.
        statements: 86,
        branches: 70,
        functions: 82,
        lines: 87,
      },
    },
  },
}));
