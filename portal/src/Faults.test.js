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


  it('arms request rejection, which is a different lever from operation failure', async () => {
    // Rejecting requests happens BEFORE a handler runs; failing operations
    // happens inside one. Two knobs, and only one had a test.
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true, status: 200, json: () => Promise.resolve({ status: 'ok' }),
    });
    render(Faults);
    const spinners = screen.getAllByRole('spinbutton');
    await fireEvent.input(spinners[1], { target: { value: '2' } });
    const arms = screen.getAllByRole('button', { name: 'Arm' });
    await fireEvent.click(arms[arms.length - 1]);
    await waitFor(() =>
      expect(screen.getByText('next 2 request(s) will be rejected')).toBeInTheDocument());
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ rejectNextRequests: 2 });
  });


  it('sends the LRO delay that was typed, not the default', async () => {
    // The input's binding: without this the field is never written to, and a
    // test that clicks Set only ever proves the default is sent.
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true, status: 200, json: () => Promise.resolve({ status: 'ok' }),
    });
    render(Faults);
    const spinners = screen.getAllByRole('spinbutton');
    await fireEvent.input(spinners[spinners.length - 1], { target: { value: '5' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Set' }));
    await waitFor(() =>
      expect(screen.getByText('operations now stay Running 5s')).toBeInTheDocument());
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ lroDelaySeconds: 5 });
  });
});
