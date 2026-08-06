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
    setupFiles: ['./vitest-setup.ts'],
    include: ['src/**/*.test.ts'],
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
        'src/main.ts',
        // Types and styles, which carry no statements to cover.
        'src/**/*.d.ts',
        'src/app.css',
        // The tests and their support module. Coverage is a statement about the
        // PRODUCT; measuring the suite against itself only ever says that the
        // tests ran, which the test count already says.
        'src/**/*.test.ts',
        'src/testing.ts',
      ],
      reporter: ['text', 'json-summary'],
      thresholds: {
        // Every statement, function and line. Exactly two statements carry a
        // `/* v8 ignore next */`, both defensive guards no input can reach
        // (a second `connect()`, and `edgePath` on a link whose node is gone),
        // each with the reason written beside it.
        statements: 100,
        functions: 100,
        lines: 100,
        // BRANCHES CANNOT REACH 100, and the reason is the compiler rather than
        // the tests. Svelte wraps every `{interpolation}` in a nullish guard, so
        // `{j.status.toLowerCase()}` compiles to a two-armed branch whose second
        // arm needs `status` to be nullish — which would have thrown inside
        // `toLowerCase()` first. 38 of the 508 arms are that shape; all of them
        // sit on markup lines with no source-level conditional at all.
        //
        // Set to the achieved figure, so covering a real arm can only raise it
        // and losing one fails the build. `vitest run --coverage` prints the
        // remaining lines if this ever needs re-deriving.
        branches: 92,
      },
    },
  },
}));
