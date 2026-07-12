import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Faults from './Faults.svelte';

describe('Faults', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('arms operation failures and confirms', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ status: 'ok' }),
    });

    render(Faults);
    const [failN] = screen.getAllByRole('spinbutton');
    await fireEvent.input(failN, { target: { value: '3' } });
    const [armFail] = screen.getAllByRole('button', { name: 'Arm' });
    await fireEvent.click(armFail);

    await waitFor(() =>
      expect(screen.getByText('next 3 operation(s) will fail')).toBeInTheDocument(),
    );
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/_emulator/faults');
    expect(JSON.parse(opts.body)).toEqual({ failNextOperations: 3 });
  });

  it('sets the LRO delay', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ status: 'ok' }),
    });

    render(Faults);
    await fireEvent.click(screen.getByRole('button', { name: 'Set' }));
    await waitFor(() =>
      expect(screen.getByText('operations now stay Running 30s')).toBeInTheDocument(),
    );
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ lroDelaySeconds: 30 });
  });

  it('surfaces rejection errors', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: false,
      status: 500,
      json: () => Promise.resolve({ error: { message: 'nope' } }),
    });
    render(Faults);
    await fireEvent.click(screen.getByRole('button', { name: 'Set' }));
    await waitFor(() => expect(screen.getByText('nope')).toBeInTheDocument());
  });
});
