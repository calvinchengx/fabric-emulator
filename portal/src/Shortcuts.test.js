import { render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Shortcuts from './Shortcuts.svelte';

const rows = [
  {
    workspaceId: 'ws-1', workspaceName: 'analytics',
    itemId: 'it-1', itemName: 'src-lh',
    path: 'Files', name: 'linked',
    targetWorkspaceId: 'ws-2', targetItemId: 'it-2', targetPath: 'Files/raw',
    dangling: false,
  },
  {
    workspaceId: 'ws-1', workspaceName: 'analytics',
    itemId: 'it-1', itemName: 'src-lh',
    path: 'Tables', name: 'gone',
    targetWorkspaceId: 'ws-2', targetItemId: 'it-9', targetPath: '',
    dangling: true,
  },
];

describe('Shortcuts', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the empty state', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ value: [] }),
    });
    render(Shortcuts);
    await waitFor(() => expect(screen.getByText(/No shortcuts yet/)).toBeInTheDocument());
  });

  it('groups shortcuts by workspace and marks dangling targets', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ value: rows }),
    });
    render(Shortcuts);
    await waitFor(() => expect(screen.getByText(/analytics/)).toBeInTheDocument());
    expect(screen.getByText('Files/linked')).toBeInTheDocument();
    expect(screen.getByText(/ws-2\/it-2\/Files\/raw/)).toBeInTheDocument();
    expect(screen.getByText('resolves')).toBeInTheDocument();
    expect(screen.getByText('dangling')).toBeInTheDocument();
  });

  it('surfaces load errors', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: false,
      status: 500,
      json: () => Promise.resolve({ error: { message: 'db gone' } }),
    });
    render(Shortcuts);
    await waitFor(() => expect(screen.getByText('db gone')).toBeInTheDocument());
  });
});
