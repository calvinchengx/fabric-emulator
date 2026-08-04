<script>
  import { api } from './api.js';
  import { href } from './router.js';

  // The model this page is FOR, by id. Addressable is the whole point: the
  // flow graph knows which node is which semantic model, and until this page
  // had an address there was nowhere for it to point.
  let { id } = $props();

  let model = $state(null);
  let error = $state('');
  let loaded = $state(false);

  api
    .get('/_emulator/portal/models')
    .then((r) => {
      const all = r.value || [];
      model = all.find((m) => m.itemId === id) || null;
      loaded = true;
    })
    .catch((e) => {
      error = e.message;
      loaded = true;
    });
</script>

<p class="crumb"><a href={href('models')}>← Semantic models</a></p>

{#if error}
  <p class="error">{error}</p>
{:else if loaded && !model}
  <!-- SAID, not blank. A detail page that renders nothing for an unknown id is
       indistinguishable from one that is still loading, and from a model that
       exists but failed to parse. A stale bookmark should say so. -->
  <h1>Model not found</h1>
  <p class="muted">
    No published semantic model has the id <code>{id}</code>. It may have been
    deleted, or this link may predate a stack restart — the emulator keeps its
    store in memory unless a volume is mounted.
  </p>
{:else if model}
  <h1>{model.displayName}</h1>
  <p class="muted">
    In <strong>{model.workspace}</strong> — tables, their columns, every
    measure's DAX, and what each table is bound to. Parsed by the same code that
    answers <code>executeQueries</code>, so this is what the emulator believes,
    not a second reading of the definition.
  </p>

  {#if model.error}
    <p class="error">{model.error}</p>
  {:else}
        <div class="detail">
          <div class="meta">
            <span><code>{model.modelName}</code></span>
            {#if model.compatibilityLevel}<span class="muted">compatibility {model.compatibilityLevel}</span>{/if}
            <span class="muted mono">{model.itemId}</span>
          </div>

          {#each model.tables as t (t.name)}
            <div class="tbl">
              <div class="tbl-head">
                <strong>{t.name}</strong>
                {#if t.mode === 'directLake'}
                  <span class="chip directlake" title="read from Delta at query time">Direct Lake</span>
                  <code class="binding">{t.binding}</code>
                {:else}
                  <span class="chip" title="rows are embedded in the definition">import</span>
                {/if}
              </div>

              <table>
                <thead>
                  <tr><th>Column</th><th>Type</th><th>Source column</th></tr>
                </thead>
                <tbody>
                  {#each t.columns as c (c.name)}
                    <tr>
                      <td><code>{c.name}</code></td>
                      <td class="muted">{c.dataType}</td>
                      <td class="muted">{c.sourceColumn === c.name ? '' : c.sourceColumn}</td>
                    </tr>
                  {/each}
                </tbody>
              </table>

              {#if t.measures.length}
                <table class="measures">
                  <thead>
                    <tr><th>Measure</th><th>DAX</th></tr>
                  </thead>
                  <tbody>
                    {#each t.measures as ms (ms.name)}
                      <tr>
                        <td><code>{ms.name}</code></td>
                        <td><code class="dax">{ms.expression}</code></td>
                      </tr>
                    {/each}
                  </tbody>
                </table>
              {/if}
            </div>
          {/each}

          {#if model.relationships.length}
            <div class="tbl">
              <div class="tbl-head"><strong>Relationships</strong></div>
              <table>
                <tbody>
                  {#each model.relationships as r (r.name)}
                    <tr>
                      <td><code>{r.from}</code></td>
                      <td class="muted">→</td>
                      <td><code>{r.to}</code></td>
                      <td class="muted">{r.name}</td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            </div>
          {/if}
        </div>
  {/if}
{/if}

<style>
  .model { border: 1px solid var(--border); border-radius: 8px; margin-bottom: 10px; }
  .model-head {
    display: flex; align-items: center; gap: 10px; width: 100%;
    padding: 10px 12px; background: none; border: 0; cursor: pointer;
    font: inherit; color: inherit; text-align: left;
  }
  .caret { display: inline-block; transition: transform 0.12s; color: var(--muted); }
  .caret.open { transform: rotate(90deg); }
  .summary { margin-left: auto; }
  .detail { padding: 0 12px 12px 12px; }
  .meta { display: flex; gap: 14px; align-items: baseline; margin: 2px 0 12px 22px; }
  .mono { font-family: ui-monospace, monospace; font-size: 11px; }
  .tbl { margin: 0 0 14px 22px; }
  .tbl-head { display: flex; align-items: center; gap: 8px; margin-bottom: 4px; }
  .binding { font-size: 12px; }
  .chip.directlake { background: color-mix(in srgb, var(--accent) 18%, transparent); }
  .measures { margin-top: 6px; }
  .dax { white-space: pre-wrap; word-break: break-word; }
  table { width: 100%; }
</style>
