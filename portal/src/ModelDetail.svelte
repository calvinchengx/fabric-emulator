<script lang="ts">
  import { api } from './api';
  import { href } from './router';

  // The model this page is FOR, by id. Addressable is the whole point: the
  // flow graph knows which node is which semantic model, and until this page
  // had an address there was nowhere for it to point.
  let { id } = $props();

  let model = $state<any>(null);
  let error = $state('');
  let loaded = $state(false);

  api
    .get('/_emulator/portal/models')
    .then((r) => {
      const all = r.value || [];
      model = all.find((m: any) => m.itemId === id) || null;
      loaded = true;
      if (model) dax = sampleQuery(model);
    })
    .catch((e) => {
      error = (e as Error).message;
      loaded = true;
    });

  // The query box. Describing a model answers "what is in it"; only running a
  // query answers "what does it say" — and this goes through the SAME
  // evaluator as executeQueries, so what works here works on the wire.
  let dax = $state('');
  let running = $state(false);
  let queryError = $state('');
  let result = $state<any>(null); // {columns: [...], rows: [...]} once a query ran

  /** A starting query derived from the model itself: its first measure over
   * the first string column — something that returns rows on the first click
   * rather than an empty editor daring the user to remember DAX. */
  function sampleQuery(m: any) {
    for (const t of m.tables || []) {
      const measure = (t.measures || [])[0];
      if (!measure) continue;
      const col = (t.columns || []).find((c: any) => c.dataType === 'string');
      const group = col ? `${t.name}[${col.name}], ` : '';
      return `EVALUATE SUMMARIZECOLUMNS(${group}"${measure.name}", [${measure.name}])`;
    }
    const first = (m.tables || [])[0];
    return first ? `EVALUATE '${first.name}'` : '';
  }

  function runQuery() {
    if (!dax.trim() || running) return;
    running = true;
    queryError = '';
    api
      .post(`/_emulator/portal/models/${encodeURIComponent(id)}/query`, { query: dax })
      .then((r) => {
        const rows = r.rows || [];
        // Column order from the rows' own keys, first-seen: DAX names columns
        // like `Country[Country]` and `[Total Revenue]`, and the rows are the
        // only place that vocabulary exists client-side.
        const cols: string[] = [];
        for (const row of rows)
          for (const k of Object.keys(row)) if (!cols.includes(k)) cols.push(k);
        result = { columns: cols, rows };
      })
      .catch((e) => {
        queryError = e.message;
        result = null;
      })
      .finally(() => (running = false));
  }
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

          <!-- `?? []` throughout: sampleQuery above already defends against a
               definition that omits an array, and the markup must agree with it.
               It did not — a table without a `measures` key reached
               `t.measures.length` and took the whole page down, while the query
               box derived from the same model perfectly happily. -->
          {#each model.tables ?? [] as t (t.name)}
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
                  {#each t.columns ?? [] as c (c.name)}
                    <tr>
                      <td><code>{c.name}</code></td>
                      <td class="muted">{c.dataType}</td>
                      <td class="muted">{c.sourceColumn === c.name ? '' : c.sourceColumn}</td>
                    </tr>
                  {/each}
                </tbody>
              </table>

              {#if t.measures?.length}
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

          <!-- The runner. Import models only: their rows live in the
               definition this page already serves, so evaluating a query is
               still a read. Direct Lake is refused server-side with the route
               that does work. -->
          <div class="tbl query-box">
            <div class="tbl-head"><strong>Query</strong>
              <span class="muted">— DAX, through the same evaluator as <code>executeQueries</code></span>
            </div>
            <textarea
              class="dax-input"
              rows="3"
              bind:value={dax}
              aria-label="DAX query"
              spellcheck="false"
            ></textarea>
            <div class="query-actions">
              <button class="run" onclick={runQuery} disabled={running || !dax.trim()}>
                {running ? 'Running…' : 'Run'}
              </button>
              {#if result}<span class="muted">{result.rows.length} row(s)</span>{/if}
            </div>
            {#if queryError}<p class="error">{queryError}</p>{/if}
            {#if result}
              {#if result.rows.length === 0}
                <p class="muted">No rows — the query ran and returned nothing.</p>
              {:else}
                <table class="query-result">
                  <thead>
                    <tr>{#each result.columns as c (c)}<th><code>{c}</code></th>{/each}</tr>
                  </thead>
                  <tbody>
                    {#each result.rows as row, i (i)}
                      <tr>{#each result.columns as c (c)}<td class="mono">{row[c] ?? ''}</td>{/each}</tr>
                    {/each}
                  </tbody>
                </table>
              {/if}
            {/if}
          </div>

          {#if model.relationships?.length}
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
  .query-box { padding-bottom: 10px; }
  .dax-input {
    width: 100%; box-sizing: border-box; margin-top: 8px;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px;
    padding: 8px 10px; border: 1px solid var(--border); border-radius: 6px;
    background: transparent; color: inherit; resize: vertical;
  }
  .query-actions { display: flex; align-items: center; gap: 10px; margin-top: 8px; }
  .run {
    font: inherit; padding: 4px 14px; border: 1px solid var(--border);
    border-radius: 6px; background: transparent; color: inherit; cursor: pointer;
  }
  .run:disabled { opacity: 0.5; cursor: default; }
  .query-result { margin-top: 8px; }
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
