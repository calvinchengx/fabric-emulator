import { render, screen, waitFor, fireEvent } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Models from './Models.svelte';

const model = {
  itemId: 'm1',
  workspaceId: 'w1',
  workspace: 'contoso-analytics',
  displayName: 'ContosoRevenue',
  modelName: 'ContosoRevenue',
  compatibilityLevel: 1550,
  format: 'TMSL',
  rowsLoaded: true,
  tables: [
    {
      name: 'Revenue',
      mode: 'directLake',
      binding: 'dbo.fct_daily_revenue',
      columns: [{ name: 'Revenue', dataType: 'double', sourceColumn: 'revenue' }],
      measures: [{ name: 'Total Revenue', expression: 'SUM(Revenue[Revenue])' }],
    },
  ],
  relationships: [{ name: 'Revenue_Customer', from: 'Revenue[Country]', to: 'Customer[Country]' }],
};

const mockList = (value) =>
  vi.spyOn(globalThis, 'fetch').mockResolvedValue({
    ok: true,
    status: 200,
    json: () => Promise.resolve({ value }),
  });

describe('Models', () => {
  beforeEach(() => vi.restoreAllMocks());

  it('lists models collapsed, with a summary of what is in each', async () => {
    mockList([model]);
    render(Models);
    await waitFor(() => expect(screen.getByText('ContosoRevenue')).toBeInTheDocument());
    // The summary is the point of the collapsed row: counts, not contents.
    //
    // This line read `1 measures` for a long time, and this test asserted it —
    // written from what the component printed rather than from the rule it was
    // meant to follow, so it froze the typo instead of catching it. Two of the
    // three counts pluralised and one did not, which is invisible until a model
    // with exactly one measure exists.
    expect(screen.getByText('1 table, 1 measure, 1 relationship')).toBeInTheDocument();
    // DAX must NOT be on screen until asked for — a page that opens with every
    // measure of every model expanded buries the list it exists to show.
    expect(screen.queryByText('SUM(Revenue[Revenue])')).not.toBeInTheDocument();
  });

  it.each([
    [1, 1, 1, '1 table, 1 measure, 1 relationship'],
    [2, 3, 0, '2 tables, 3 measures, 0 relationships'],
  ])('pluralises every count independently (%i/%i/%i)', async (nt, nm, nr, want) => {
    // All three counts, at one and at not-one. A single fixture can only ever
    // exercise one side of each ternary, which is how `1 measures` survived.
    const tables = Array.from({ length: nt }, (_, i) => ({
      name: `T${i}`,
      mode: 'import',
      columns: [],
      measures: i === 0 ? Array.from({ length: nm }, (_, j) => ({ name: `M${j}`, expression: 'X' })) : [],
    }));
    mockList([
      {
        ...model,
        itemId: `m-${nt}-${nm}-${nr}`,
        tables,
        relationships: Array.from({ length: nr }, (_, i) => ({ name: `R${i}`, from: 'a', to: 'b' })),
      },
    ]);
    render(Models);
    await waitFor(() => expect(screen.getByText(want)).toBeInTheDocument());
  });

  it('links each row to the model\'s own address', async () => {
    mockList([model]);
    render(Models);
    const row = await screen.findByRole('link', { name: /ContosoRevenue/ });
    // An href, not an onclick. The whole reason this stopped being an accordion
    // is that a detail view with no URL cannot be linked to, opened in a new
    // tab, or pointed at from the flow graph.
    expect(row.getAttribute('href')).toBe('#models/m1');
  });

  it('renders the columns, the DAX and the binding at that address', async () => {
    mockList([model]);
    render(Models, { props: { id: 'm1' } });
    await waitFor(() => expect(screen.getByText('SUM(Revenue[Revenue])')).toBeInTheDocument());
    expect(screen.getByText('dbo.fct_daily_revenue')).toBeInTheDocument();
    expect(screen.getByText('Direct Lake')).toBeInTheDocument();
    expect(screen.getByText('Revenue[Country]')).toBeInTheDocument();
  });

  it('says so when the id matches no model, rather than rendering blank', async () => {
    mockList([model]);
    render(Models, { props: { id: 'gone' } });
    // A blank detail page is indistinguishable from one still loading and from
    // a model that exists but failed to parse. A stale bookmark — this store is
    // in memory unless a volume is mounted — must say what happened.
    await waitFor(() => expect(screen.getByText('Model not found')).toBeInTheDocument());
    expect(screen.getByText(/gone/)).toBeInTheDocument();
  });

  it('shows an unreadable model instead of dropping it', async () => {
    mockList([
      {
        itemId: 'm2',
        workspace: 'w',
        displayName: 'broken',
        error: 'invalid TMSL model: unexpected end of JSON input',
        tables: [],
        relationships: [],
      },
    ]);
    render(Models);
    // The item is named and marked, not absent: a model missing from the list
    // reads as "never published", which sends you to the wrong problem.
    await waitFor(() => expect(screen.getByText('broken')).toBeInTheDocument());
    expect(screen.getByText('unreadable')).toBeInTheDocument();
  });

  it('flags an import model carrying no rows', async () => {
    mockList([{ ...model, rowsLoaded: false }]);
    render(Models);
    // Without data.json every DAX query answers empty, which is indis-
    // tinguishable from a wrong measure until someone tells you.
    await waitFor(() => expect(screen.getByText('no rows')).toBeInTheDocument());
  });

  it('says so when nothing is published', async () => {
    mockList([]);
    render(Models);
    await waitFor(() =>
      expect(screen.getByText('No semantic models published yet.')).toBeInTheDocument(),
    );
  });
});

describe('Models accessibility', () => {
  beforeEach(() => vi.restoreAllMocks());

  // The row's own text is chips and counts. A screen reader reading it out
  // announces everything except what the control does — which the a11y tree
  // showed as an unnamed button.
  it('gives each row an accessible name that is the model, not its chips', async () => {
    mockList([model]);
    render(Models);
    // The row's text is a pile of chips and counts, so a screen reader
    // announcing it raw says everything except which model it is.
    const row = await screen.findByRole('link', { name: /ContosoRevenue/ });
    expect(row).toBeInTheDocument();
  });

  it('offers a way back from the detail page', async () => {
    mockList([model]);
    render(Models, { props: { id: 'm1' } });
    // A page reached by URL may be someone's entry point to the whole portal;
    // without this the only way out is the browser's back button, which does
    // not exist if the link was opened in a new tab.
    const back = await screen.findByRole('link', { name: /Semantic models/ });
    expect(back.getAttribute('href')).toBe('#models');
  });
});
