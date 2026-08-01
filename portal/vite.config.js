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
    include: ['src/**/*.test.js'],
  },
}));
