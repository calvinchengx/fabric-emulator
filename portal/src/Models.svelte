<script>
  import { api } from './api.js';

  let models = $state(null);
  let error = $state('');
  // Which model is expanded. Collapsed by default because the interesting
  // thing at a glance is the LIST — how many models, on which target, bound
  // how — and a page that opens with every measure of every model expanded
  // buries that under DAX.
  let open = $state(null);

  api
    .get('/_emulator/portal/models')
    .then((r) => (models = r.value || []))
    .catch((e) => (error = e.message));

  const toggle = (id) => (open = open === id ? null : id);

  // A table's row count is not knowable from the definition — an import
  // table's rows live in data.json and a Direct Lake table's live in Delta —
  // so the summary counts what the DEFINITION actually states.
  const summary = (m) =>
    `${m.tables.length} table${m.tables.length === 1 ? '' : 's'}, ` +
    `${m.tables.reduce((n, t) => n + t.measures.length, 0)} measures, ` +
    `${m.relationships.length} relationship${m.relationships.length === 1 ? '' : 's'}`;
</script>

<h1>Semantic models</h1>
<p class="muted">
  What each published model actually contains — tables, their columns, every
  measure's DAX, and what each table is bound to. Parsed by the same code that
  answers <code>executeQueries</code>, so this is what the emulator believes,
  not a second reading of the definition.
</p>

{#if error}<p class="error">{error}</p>{/if}

{#if models && models.length === 0}
  <p class="muted">No semantic models published yet.</p>
{/if}

{#each models || [] as m (m.itemId)}
  <div class="model">
    <!-- The label is explicit because the row's own text is a pile of chips
         and counts: a screen reader announcing "ContosoRevenue contoso-
         analytics TMSL rows loaded 2 tables, 3 measures" says everything
         except what the control DOES. -->
    <button
      class="model-head"
      onclick={() => toggle(m.itemId)}
      aria-expanded={open === m.itemId}
      aria-label={`${open === m.itemId ? 'Collapse' : 'Expand'} ${m.displayName}`}
    >
      <span class="caret" class:open={open === m.itemId}>▸</span>
      <strong>{m.displayName}</strong>
      <span class="muted">{m.workspace}</span>
      {#if m.error}
        <span class="chip failed" title={m.error}>unreadable</span>
      {:else}
        <span class="chip">{m.format}</span>
        {#if m.rowsLoaded}
          <span class="chip succeeded" title="an inline data.json snapshot is present">rows loaded</span>
        {:else}
          <span class="chip" title="no data.json — an import model with no rows answers every query with nothing">no rows</span>
        {/if}
        <span class="muted summary">{summary(m)}</span>
      {/if}
    </button>

    {#if open === m.itemId}
      {#if m.error}
        <!-- Shown, not hidden. A model that vanishes from a list reads as
             "never published", which is a different problem to diagnose. -->
        <p class="error detail">{m.error}</p>
      {:else}
        <div class="detail">
          <div class="meta">
            <span><code>{m.modelName}</code></span>
            {#if m.compatibilityLevel}<span class="muted">compatibility {m.compatibilityLevel}</span>{/if}
            <span class="muted mono">{m.itemId}</span>
          </div>

          {#each m.tables as t (t.name)}
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

          {#if m.relationships.length}
            <div class="tbl">
              <div class="tbl-head"><strong>Relationships</strong></div>
              <table>
                <tbody>
                  {#each m.relationships as r (r.name)}
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
  </div>
{/each}

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
