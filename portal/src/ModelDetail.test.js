import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import ModelDetail from './ModelDetail.svelte';

// ModelDetail was only ever reached THROUGH Models, so its own arms went
// untested: the query box's guards, the two table modes, and every "this model
// does not have that" case. It is the page a flow-graph node links to, which
// makes it the one page a stranger arrives at cold.

const table = (over = {}) => ({
  name: 'Revenue',
  mode: 'import',
  columns: [{ name: 'Country', dataType: 'string', sourceColumn: 'Country' }],
  measures: [{ name: 'Total Revenue', expression: 'SUM(Revenue[Amount])' }],
  ...over,
});

const model = (over = {}) => ({
  itemId: 'm1',
  workspace: 'contoso-analytics',
  displayName: 'ContosoRevenue',
  modelName: 'ContosoRevenue',
  compatibilityLevel: 1550,
  tables: [table()],
  relationships: [],
  ...over,
});

/** The listing this page filters by id, plus an optional query response. */
function mockApi({ models = [model()], query, queryFails } = {}) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((url, opts) => {
    if (opts?.method === 'POST') {
      return queryFails
        ? Promise.resolve({ ok: false, status: 400,
            json: () => Promise.resolve({ error: { message: queryFails } }) })
        : Promise.resolve({ ok: true, status: 200,
            json: () => Promise.resolve(query ?? { rows: [] }) });
    }
    return Promise.resolve({ ok: true, status: 200,
      json: () => Promise.resolve({ value: models }) });
  });
}

const show = (id = 'm1') => render(ModelDetail, { props: { id } });

describe('ModelDetail', () => {
  beforeEach(() => vi.restoreAllMocks());

  it('surfaces a failed listing rather than claiming the model is missing', async () => {
    // Two different sentences: "the store would not answer" and "no such
    // model". Collapsing them sends someone looking for a deleted model.
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: false, status: 500,
      json: () => Promise.resolve({ error: { message: 'store unavailable' } }),
    });
    show();
    await waitFor(() => expect(screen.getByText('store unavailable')).toBeInTheDocument());
    expect(screen.queryByText('Model not found')).not.toBeInTheDocument();
  });

  it('says a stale link names nothing, rather than rendering blank', async () => {
    mockApi({ models: [model()] });
    show('gone');
    await waitFor(() => expect(screen.getByText('Model not found')).toBeInTheDocument());
  });

  it('shows a model the emulator could not parse', async () => {
    mockApi({ models: [model({ error: 'unsupported TMSL: missing tables' })] });
    show();
    await waitFor(() =>
      expect(screen.getByText('unsupported TMSL: missing tables')).toBeInTheDocument());
    // The tables section must not render off a model that failed to parse.
    expect(screen.queryByRole('button', { name: 'Run' })).not.toBeInTheDocument();
  });

  describe('the table header', () => {
    it('names the Delta binding for a Direct Lake table', async () => {
      mockApi({ models: [model({ tables: [table({
        mode: 'directLake', binding: 'dbo.fct_daily_revenue' })] })] });
      show();
      await waitFor(() => expect(screen.getByText('Direct Lake')).toBeInTheDocument());
      expect(screen.getByText('dbo.fct_daily_revenue')).toBeInTheDocument();
      expect(screen.queryByText('import')).not.toBeInTheDocument();
    });

    it('marks an import table as carrying its own rows', async () => {
      mockApi();
      show();
      await waitFor(() => expect(screen.getByText('import')).toBeInTheDocument());
      expect(screen.queryByText('Direct Lake')).not.toBeInTheDocument();
    });

    it('omits the compatibility level when the model does not state one', async () => {
      mockApi({ models: [model({ compatibilityLevel: undefined })] });
      show();
      // By role: the display name and the model name are the same string here,
      // so a text query matches the heading and the `<code>` beside it.
      await waitFor(() => expect(
        screen.getByRole('heading', { level: 1, name: 'ContosoRevenue' })).toBeInTheDocument());
      expect(screen.queryByText(/compatibility/)).not.toBeInTheDocument();
    });
  });

  it('leaves the source column blank when it is the same name twice', async () => {
    // Printing `Country` twice on one row is noise; the column only matters
    // when the model renamed something.
    mockApi({ models: [model({ tables: [table({ columns: [
      { name: 'Country', dataType: 'string', sourceColumn: 'Country' },
      { name: 'Amount', dataType: 'double', sourceColumn: 'revenue_amount' },
    ] })] })] });
    show();
    await waitFor(() => expect(screen.getByText('revenue_amount')).toBeInTheDocument());
    expect(screen.queryAllByText('Country')).toHaveLength(1);
  });

  it('omits the measures table for a table that has none', async () => {
    mockApi({ models: [model({ tables: [table({ measures: [] })] })] });
    show();
    await waitFor(() => expect(screen.getByText('Revenue')).toBeInTheDocument());
    expect(screen.queryByText('DAX')).not.toBeInTheDocument();
  });

  it('omits the relationships section when there are none', async () => {
    mockApi();
    show();
    await waitFor(() => expect(screen.getByText('Revenue')).toBeInTheDocument());
    expect(screen.queryByText('Relationships')).not.toBeInTheDocument();
  });

  it('lists relationships when the model has them', async () => {
    mockApi({ models: [model({ relationships: [
      { name: 'r1', from: 'Revenue[Country]', to: 'Customer[Country]' }] })] });
    show();
    await waitFor(() => expect(screen.getByText('Relationships')).toBeInTheDocument());
    expect(screen.getByText('Revenue[Country]')).toBeInTheDocument();
  });

  describe('the starting query, derived from the model', () => {
    const daxBox = () => screen.getByLabelText('DAX query');

    it('groups by the first string column of the first table that has a measure', async () => {
      mockApi();
      show();
      await waitFor(() => expect(daxBox()).toHaveValue(
        'EVALUATE SUMMARIZECOLUMNS(Revenue[Country], "Total Revenue", [Total Revenue])'));
    });

    it('skips a table with no measures and uses the next one that has some', async () => {
      // The `continue` arm. A dimension table usually has no measures and is
      // usually first, so without this the box starts on the wrong table.
      mockApi({ models: [model({ tables: [
        table({ name: 'Customer', measures: [] }),
        table({ name: 'Revenue' }),
      ] })] });
      show();
      await waitFor(() => expect(daxBox()).toHaveValue(
        'EVALUATE SUMMARIZECOLUMNS(Revenue[Country], "Total Revenue", [Total Revenue])'));
    });

    it('omits the grouping column when no column is a string', async () => {
      mockApi({ models: [model({ tables: [table({
        columns: [{ name: 'Amount', dataType: 'double', sourceColumn: 'Amount' }] })] })] });
      show();
      await waitFor(() => expect(daxBox()).toHaveValue(
        'EVALUATE SUMMARIZECOLUMNS("Total Revenue", [Total Revenue])'));
    });

    it('falls back to evaluating the first table when nothing has a measure', async () => {
      mockApi({ models: [model({ tables: [table({ measures: [] })] })] });
      show();
      await waitFor(() => expect(daxBox()).toHaveValue("EVALUATE 'Revenue'"));
    });

    it('leaves the box empty, and Run disabled, for a model with no tables', async () => {
      mockApi({ models: [model({ tables: [] })] });
      show();
      await waitFor(() => expect(daxBox()).toHaveValue(''));
      expect(screen.getByRole('button', { name: 'Run' })).toBeDisabled();
    });
  });

  describe('running a query', () => {
    it('renders the rows, with columns in the order the rows name them', async () => {
      mockApi({ query: { rows: [
        { 'Revenue[Country]': 'US', '[Total Revenue]': 100 },
        { 'Revenue[Country]': 'DE', '[Total Revenue]': 50 },
      ] } });
      show();
      await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeEnabled());
      await fireEvent.click(screen.getByRole('button', { name: 'Run' }));
      await waitFor(() => expect(screen.getByText('2 row(s)')).toBeInTheDocument());
      expect(screen.getByText('US')).toBeInTheDocument();
      expect(screen.getByText('100')).toBeInTheDocument();
    });

    it('blanks a cell the row simply does not have', async () => {
      // DAX can return heterogeneous rows; `row[c] ?? ''` is what stops
      // "undefined" appearing in a result table.
      mockApi({ query: { rows: [
        { 'Revenue[Country]': 'US', '[Total Revenue]': 100 },
        { 'Revenue[Country]': 'DE' },
      ] } });
      show();
      await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeEnabled());
      await fireEvent.click(screen.getByRole('button', { name: 'Run' }));
      await waitFor(() => expect(screen.getByText('2 row(s)')).toBeInTheDocument());
      expect(screen.getByText('DE')).toBeInTheDocument();
      expect(screen.queryByText('undefined')).not.toBeInTheDocument();
    });

    it('says a query that returned nothing RAN, rather than showing an empty table', async () => {
      mockApi({ query: { rows: [] } });
      show();
      await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeEnabled());
      await fireEvent.click(screen.getByRole('button', { name: 'Run' }));
      await waitFor(() => expect(
        screen.getByText('No rows — the query ran and returned nothing.')).toBeInTheDocument());
    });

    it('treats a response with no rows key as no rows', async () => {
      mockApi({ query: {} });
      show();
      await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeEnabled());
      await fireEvent.click(screen.getByRole('button', { name: 'Run' }));
      await waitFor(() => expect(screen.getByText('0 row(s)')).toBeInTheDocument());
    });

    it('shows the evaluator message and drops the previous result', async () => {
      // For an interactive box the message IS the product: "syntax error near
      // SUMMARIZE" is the whole reason to type in a browser instead of curl.
      mockApi({ queryFails: 'unexpected token at position 9' });
      show();
      await waitFor(() => expect(screen.getByRole('button', { name: 'Run' })).toBeEnabled());
      await fireEvent.click(screen.getByRole('button', { name: 'Run' }));
      await waitFor(() =>
        expect(screen.getByText('unexpected token at position 9')).toBeInTheDocument());
      expect(screen.queryByText(/row\(s\)/)).not.toBeInTheDocument();
    });

    it('does nothing when the box holds only whitespace', async () => {
      // The guard that stops an empty POST. Asserted through the request count,
      // because the visible effect of doing nothing is nothing.
      mockApi();
      show();
      const box = await waitFor(() => screen.getByLabelText('DAX query'));
      await fireEvent.input(box, { target: { value: '   ' } });
      const before = globalThis.fetch.mock.calls.length;
      await fireEvent.click(screen.getByRole('button', { name: 'Run' }));
      expect(globalThis.fetch.mock.calls).toHaveLength(before);
    });
  });

  it('offers a way back to the listing', async () => {
    mockApi();
    show();
    const back = await screen.findByRole('link', { name: '← Semantic models' });
    expect(back).toHaveAttribute('href', '#models');
  });


  it('survives a model whose optional arrays are absent entirely', async () => {
    // `m.tables || []`, `t.measures || []`, `t.columns || []` — three fallbacks
    // for a definition that simply omits a key. Without them the page throws
    // while deriving its own starting query.
    mockApi({ models: [{ itemId: 'm1', workspace: 'w', displayName: 'Sparse',
                         modelName: 'Sparse', relationships: [] }] });
    show();
    await waitFor(() => expect(
      screen.getByRole('heading', { level: 1, name: 'Sparse' })).toBeInTheDocument());
    expect(screen.getByLabelText('DAX query')).toHaveValue('');
  });

  it('handles a table that omits its columns and measures', async () => {
    // Keys ABSENT, not empty: `t.measures || []` and `t.columns || []` only
    // matter when the definition leaves them out entirely.
    mockApi({ models: [{ itemId: 'm1', workspace: 'w', displayName: 'Bare',
                         modelName: 'Bare', relationships: [],
                         tables: [{ name: 'Lonely', mode: 'import' }] }] });
    show();
    await waitFor(() => expect(screen.getByText('Lonely')).toBeInTheDocument());
    expect(screen.getByLabelText('DAX query')).toHaveValue("EVALUATE 'Lonely'");
  });

  it('treats a listing with no value key as no models', async () => {
    // `r.value || []`. Without the fallback, `.find` runs on undefined and the
    // page throws instead of saying the id names nothing.
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true, status: 200, json: () => Promise.resolve({}),
    });
    show();
    await waitFor(() => expect(screen.getByText('Model not found')).toBeInTheDocument());
  });

  it('derives a query for a table with measures but no columns key', async () => {
    // `(t.columns || [])` is only reached once a measure was found, so the
    // fallback needs a table that has measures and omits columns entirely.
    mockApi({ models: [model({ tables: [{ name: 'Facts', mode: 'import',
      measures: [{ name: 'Total', expression: 'SUM(x)' }] }] })] });
    show();
    await waitFor(() => expect(screen.getByLabelText('DAX query')).toHaveValue(
      'EVALUATE SUMMARIZECOLUMNS("Total", [Total])'));
  });
});
