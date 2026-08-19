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
    // The parity map dropped its reading-order number (it's a living
    // reference, not a chapter) and now lives at /parity/.
    '/17-parity/': '/fabric-emulator/parity/',
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
        // Top nav: the parity version picker, rendered beside the search box.
        // Search occupies the header's un-gated middle slot, so the picker stays
        // in the top bar at every width (the right-group that holds ThemeSelect
        // is `sl-hidden md:sl-flex`, so a picker there vanishes on mobile and
        // resurfaces in the mobile menu footer). The picker shows itself only on
        // the parity pages.
        Search: './src/components/Search.astro',
      },
      description:
        'A local emulator of Microsoft Fabric — control plane, OneLake, Spark, T-SQL, pipelines, Eventstream, and KQL — that validates Entra bearer tokens.',
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/calvinchengx/fabric-emulator' },
      ],
      editLink: {
        baseUrl: 'https://github.com/calvinchengx/fabric-emulator/edit/main/docs/',
      },
      sidebar: [
        {
          label: 'Getting started',
          items: [
            { slug: 'index' },
            { slug: '01-quickstart' },
            { slug: '02-installation' },
            { slug: '26-platform-setup' },
            { slug: '27-running-modes' },
            { slug: '03-architecture' },
            { slug: '04-configuration' },
            { slug: '05-tls-and-hosts' },
          ],
        },
        {
          label: 'Reference',
          items: [
            { slug: '06-data-model-and-seed' },
            { slug: '07-control-plane-api' },
            { slug: '08-onelake' },
            { slug: '09-identity-handshake' },
          ],
        },
        {
          label: 'Tutorials',
          items: [
            { slug: '28-tutorial-end-to-end' },
          ],
        },
        {
          label: 'Testing',
          items: [
            { slug: '10-testing' },
            { slug: '11-testing-with-fabric-cicd' },
            { slug: '12-e2e-matrix' },
          ],
        },
        {
          label: 'Project',
          items: [
            { slug: '13-roadmap' },
            { slug: '14-real-compute' },
            { slug: '15-entra-companion' },
            { slug: '16-warehouse-tds' },
            { slug: '18-semantic-model-references' },
            { slug: '19-semantic-model-plan' },
            { slug: '20-lakesail-engine' },
            { slug: '21-real-fabric-toggle' },
            { slug: '46-artifact-persistence' },
            { slug: '22-openmetadata' },
            { slug: '23-deployment-pipelines' },
            { slug: '24-parity-completion' },
            { slug: '25-rti-kusto' },
            { slug: '51-eventstream-kafka' },
            { slug: '29-tsql-parity' },
            { slug: '30-odcs-data-contracts' },
            { slug: '53-dbt-expectations' },
            { slug: '31-flow-observability' },
            { slug: '32-xmla-plan' },
            { slug: '33-pbix-tooling' },
            { slug: '52-msmdsrv-hosts' },
            { slug: '34-fab-driven-example' },
            { slug: '35-warehouse-time-travel' },
            { slug: '36-capacity-job-queueing' },
            { slug: '37-runtime-fidelity-gaps' },
            { slug: '38-framework-conformance' },
            { slug: '39-run-multiple-parity-plan' },
            { slug: '40-rest-connector-plan' },
            { slug: '41-salesforce-connector-plan' },
            { slug: '42-sail-fidelity-plan' },
            { slug: '43-activity-completion-plan' },
            { slug: '44-interaction-surfaces' },
            { slug: '45-powerbi-reverse-engineering' },
            { slug: '47-environment-abstraction' },
            { slug: '48-variable-libraries' },
            { slug: '49-async-outcome-audit' },
            { slug: '50-rdd-usage-capture' },
            { slug: 'engine-matrix' },
          ],
        },
        {
          // The live map first, then the pages generated by
          // scripts/parity-versions.mjs from the git release tags. The history
          // index lists every per-version snapshot, so it doubles as the
          // version browser (this Starlight's sidebar has no autogenerate).
          // The map's own title is long ("Feature parity: fabric-emulator vs.
          // real Microsoft Fabric"); in this group it just needs to say Parity.
          label: 'Parity',
          items: [
            { slug: 'parity', label: 'Parity' },
            { slug: 'parity-history' },
            { slug: 'parity-history/changelog' },
          ],
        },
      ],
    }),
  ],
});
