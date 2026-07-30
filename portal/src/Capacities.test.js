import { render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Capacities from './Capacities.svelte';

const cap = {
  id: 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee',
  displayName: 'Emulator Capacity',
  sku: 'F64',
  region: 'local',
  state: 'Active',
  workspaces: [{ id: 'ws-1', displayName: 'analytics' }],
};

describe('Capacities', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the seeded capacity with its assignments', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ value: [cap] }),
    });
    render(Capacities);
    await waitFor(() => expect(screen.getByText('Emulator Capacity')).toBeInTheDocument());
    expect(screen.getByText('F64')).toBeInTheDocument();
    expect(screen.getByText('Active')).toBeInTheDocument();
    expect(screen.getByText('analytics')).toBeInTheDocument();
  });

  it('shows an empty assignment list', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ value: [{ ...cap, workspaces: [] }] }),
    });
    render(Capacities);
    await waitFor(() => expect(screen.getByText('none')).toBeInTheDocument());
  });

  it('surfaces load errors', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: false,
      status: 500,
      json: () => Promise.resolve({ error: { message: 'db gone' } }),
    });
    render(Capacities);
    await waitFor(() => expect(screen.getByText('db gone')).toBeInTheDocument());
  });
});
