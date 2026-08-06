import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import Clock from './Clock.svelte';

function jsonResponse(body) {
  return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });
}

describe('Clock', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('shows the clock state and freezes it', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockImplementation((url, opts) => {
      if (!opts || !opts.method || opts.method === 'GET') {
        return jsonResponse({ offset: 0, frozen: false, now: 1700000000 });
      }
      return jsonResponse({ offset: 0, frozen: true, now: 1700000000 });
    });

    render(Clock);
    await waitFor(() => expect(screen.getByText('running')).toBeInTheDocument());

    await fireEvent.click(screen.getByRole('button', { name: 'Freeze' }));
    await waitFor(() => expect(screen.getByText('frozen')).toBeInTheDocument());

    const postCall = fetchMock.mock.calls.find(([, o]) => o?.method === 'POST');
    expect(postCall[0]).toBe('/_emulator/clock');
    expect(JSON.parse(postCall[1].body)).toEqual({ freeze: true });
  });

  it('advances by the entered amount', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockImplementation((url, opts) => {
      if (!opts || !opts.method || opts.method === 'GET') {
        return jsonResponse({ offset: 0, frozen: true, now: 1700000000 });
      }
      return jsonResponse({ offset: 601, frozen: true, now: 1700000601 });
    });

    render(Clock);
    await waitFor(() => expect(screen.getByText('frozen')).toBeInTheDocument());

    const input = screen.getByRole('spinbutton');
    await fireEvent.input(input, { target: { value: '601' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Advance' }));

    await waitFor(() => expect(screen.getByText('601s')).toBeInTheDocument());
    const postCall = fetchMock.mock.calls.find(([, o]) => o?.method === 'POST');
    expect(JSON.parse(postCall[1].body)).toEqual({ advance: 601 });
  });

  it('surfaces API errors', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: false,
      status: 500,
      json: () => Promise.resolve({ error: { message: 'boom' } }),
    });
    render(Clock);
    await waitFor(() => expect(screen.getByText('boom')).toBeInTheDocument());
  });


  it('unfreezes a frozen clock', async () => {
    // The frozen state renders a different button and a different label. Only
    // the running state had ever been rendered.
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true, status: 200,
      json: () => Promise.resolve({ now: 1700000000, offset: 0, frozen: true }),
    });
    render(Clock);
    await waitFor(() => expect(screen.getByText('frozen')).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Unfreeze' }));
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(1));
    expect(JSON.parse(fetchMock.mock.calls.at(-1)[1].body)).toEqual({ freeze: false });
  });

  it('resets the offset and unfreezes in one call', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true, status: 200,
      json: () => Promise.resolve({ now: 1700000000, offset: 90, frozen: false }),
    });
    render(Clock);
    await waitFor(() => expect(screen.getByText('90s')).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Reset' }));
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(1));
    expect(JSON.parse(fetchMock.mock.calls.at(-1)[1].body)).toEqual({ offset: 0, freeze: false });
  });

  it('surfaces a failed mutation without losing the clock it is showing', async () => {
    let call = 0;
    vi.spyOn(globalThis, 'fetch').mockImplementation(() => {
      call += 1;
      return call === 1
        ? Promise.resolve({ ok: true, status: 200,
            json: () => Promise.resolve({ now: 1700000000, offset: 0, frozen: false }) })
        : Promise.resolve({ ok: false, status: 409,
            json: () => Promise.resolve({ error: { message: 'clock is held' } }) });
    });
    render(Clock);
    await waitFor(() => expect(screen.getByText('running')).toBeInTheDocument());
    await fireEvent.click(screen.getByRole('button', { name: 'Freeze' }));
    await waitFor(() => expect(screen.getByText('clock is held')).toBeInTheDocument());
    // Still showing the last known clock rather than blanking the cards.
    expect(screen.getByText('running')).toBeInTheDocument();
  });
});
