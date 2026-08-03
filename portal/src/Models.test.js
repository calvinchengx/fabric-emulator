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
    expect(screen.getByText('1 table, 1 measures, 1 relationship')).toBeInTheDocument();
    // DAX must NOT be on screen until asked for — a page that opens with every
    // measure of every model expanded buries the list it exists to show.
    expect(screen.queryByText('SUM(Revenue[Revenue])')).not.toBeInTheDocument();
  });

  it('expands to the columns, the DAX and the binding', async () => {
    mockList([model]);
    render(Models);
    await waitFor(() => expect(screen.getByText('ContosoRevenue')).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button'));

    expect(screen.getByText('SUM(Revenue[Revenue])')).toBeInTheDocument();
    expect(screen.getByText('dbo.fct_daily_revenue')).toBeInTheDocument();
    expect(screen.getByText('Direct Lake')).toBeInTheDocument();
    expect(screen.getByText('Revenue[Country]')).toBeInTheDocument();
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
  it('names the toggle and says which way it goes', async () => {
    mockList([model]);
    render(Models);
    const btn = await screen.findByRole('button', { name: 'Expand ContosoRevenue' });
    expect(btn).toHaveAttribute('aria-expanded', 'false');
    await fireEvent.click(btn);
    expect(
      await screen.findByRole('button', { name: 'Collapse ContosoRevenue' }),
    ).toHaveAttribute('aria-expanded', 'true');
  });
});
