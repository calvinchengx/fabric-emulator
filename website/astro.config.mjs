import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { remarkMermaid } from './plugins/remark-mermaid.mjs';

// Project GitHub Pages site: https://calvinchengx.github.io/fabric-emulator/
export default defineConfig({
  site: 'https://calvinchengx.github.io',
  base: '/fabric-emulator/',
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
          label: 'Documentation',
          items: [{ slug: 'index' }, { slug: '01-architecture' }, { slug: '02-api-surface' }],
        },
        {
          label: 'Roadmap',
          items: [{ slug: '03-roadmap' }],
        },
      ],
    }),
  ],
});
