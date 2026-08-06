import { render, screen, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Connections from './Connections.svelte';

const rows = [
  { id: 'c-1', displayName: 'sql-basic', connectivityType: 'ShareableCloud', credentialType: 'Basic', connectionEncryption: 'NotEncrypted' },
  { id: 'c-2', displayName: 'vault-ref', connectivityType: 'ShareableCloud', credentialType: 'AzureKeyVaultReference' },
  { id: 'c-3', displayName: 'ws-ident', connectivityType: 'ShareableCloud', credentialType: 'WorkspaceIdentity' },
];

describe('Connections', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the empty state', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ value: [] }),
    });
    render(Connections);
    await waitFor(() => expect(screen.getByText(/No connections yet/)).toBeInTheDocument());
  });

  it('lists connections and flags reference credential kinds', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ value: rows }),
    });
    render(Connections);
    await waitFor(() => expect(screen.getByText('sql-basic')).toBeInTheDocument());
    expect(screen.getByText(/Basic/)).toBeInTheDocument();
    expect(screen.getByText('NotEncrypted')).toBeInTheDocument();
    // The reference kinds carry a flag chip.
    expect(screen.getByText('Key Vault ref')).toBeInTheDocument();
    expect(screen.getByText('Workspace identity')).toBeInTheDocument();
  });

  it('surfaces load errors', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: false,
      status: 500,
      json: () => Promise.resolve({ error: { message: 'db gone' } }),
    });
    render(Connections);
    await waitFor(() => expect(screen.getByText('db gone')).toBeInTheDocument());
  });


  it('dashes the fields a connection may simply not have', async () => {
    // Every one of these is optional on the wire. Rendering `undefined` in a
    // table cell is the failure mode, and it looks like a real value.
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true, status: 200,
      json: () => Promise.resolve({ value: [{ id: 'c-bare', displayName: 'bare' }] }),
    });
    render(Connections);
    await screen.findByText('bare');
    expect(screen.getAllByText('—')).toHaveLength(3);
  });

  it('leaves an ordinary credential kind unflagged', async () => {
    // The flag means "this references another emulated system". A Basic
    // credential references nothing, so a chip here would be a lie.
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true, status: 200,
      json: () => Promise.resolve({ value: [{
        id: 'c-1', displayName: 'plain', connectivityType: 'ShareableCloud',
        credentialType: 'Basic', connectionEncryption: 'NotEncrypted',
      }] }),
    });
    render(Connections);
    await screen.findByText('plain');
    expect(screen.getByText('Basic')).toBeInTheDocument();
    expect(screen.queryByText('Key Vault ref')).not.toBeInTheDocument();
    expect(screen.queryByText('Workspace identity')).not.toBeInTheDocument();
  });
});
