import { render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Warehouse from './Warehouse.svelte';

describe('Warehouse', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('reports an unconfigured endpoint', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ sqlTdsConfigured: false, warehouseSqlConfigured: false, tdsListener: 'off' }),
    });
    render(Warehouse);
    await waitFor(() => expect(screen.getByText('FABRIC_SQL_TDS_ADDR')).toBeInTheDocument());
    expect(screen.getAllByText('not configured')).toHaveLength(2);
    expect(screen.getByText('off')).toBeInTheDocument();
  });

  it('reports a relay-configured endpoint without echoing values', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ sqlTdsConfigured: true, warehouseSqlConfigured: true, tdsListener: 'relay' }),
    });
    const { container } = render(Warehouse);
    await waitFor(() => expect(screen.getAllByText('configured')).toHaveLength(2));
    expect(screen.getByText('relay')).toBeInTheDocument();
    expect(screen.getByText(/queries run on the configured SQL Server/)).toBeInTheDocument();
    // Presence only — no address or DSN anywhere in the view.
    expect(container.textContent).not.toMatch(/1433|sqlserver:|:\/\//);
  });

  it('surfaces load errors', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: false,
      status: 500,
      json: () => Promise.resolve({ error: { message: 'db gone' } }),
    });
    render(Warehouse);
    await waitFor(() => expect(screen.getByText('db gone')).toBeInTheDocument());
  });
});
