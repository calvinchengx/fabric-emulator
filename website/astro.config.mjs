import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { remarkMermaid } from './plugins/remark-mermaid.mjs';

// Project GitHub Pages site: https://calvinchengx.github.io/fabric-emulator/
export default defineConfig({
  site: 'https://calvinchengx.github.io',
  base: '/fabric-emulator/',
  // Docs were renumbered into reading order; keep the old published URLs alive.
  redirects: {
    '/01-architecture/': '/fabric-emulator/03-architecture/',
    '/02-api-surface/': '/fabric-emulator/07-control-plane-api/',
    '/03-roadmap/': '/fabric-emulator/13-roadmap/',
    '/04-real-compute/': '/fabric-emulator/14-real-compute/',
  },
  // remarkMermaid turns ```mermaid fences into <pre class="mermaid"> before
  // Expressive Code sees them; src/components/Head.astro renders them client-side.
  markdown: {
    remarkPlugins: [remarkMermaid],
  },
  integrations: [
    starlight({
      title: 'Fabric Emulator',
      components: {
        Head: './src/components/Head.astro',
      },
      description:
        'A local emulator of the Microsoft Fabric control plane — workspaces, items, RBAC, git integration, and long-running operations — that validates Entra bearer tokens.',
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/calvinchengx/fabric-emulator' },
      ],
      editLink: {
        baseUrl: 'https://github.com/calvinchengx/fabric-emulator/edit/main/docs/',
      },
      sidebar: [
        {
          label: 'Getting started',
          items: [{ slug: 'index' }, { slug: '03-architecture' }],
        },
        {
          label: 'Reference',
          items: [{ slug: '07-control-plane-api' }, { slug: '08-onelake' }],
        },
        {
          label: 'Project',
          items: [{ slug: '13-roadmap' }, { slug: '14-real-compute' }],
        },
      ],
    }),
  ],
});
