import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { api, copy } from './api.js';

// Build a fake fetch Response.
function res(status, body, ok = status < 400) {
  return {
    status,
    ok,
    json: () => Promise.resolve(body),
  };
}

describe('api client', () => {
  beforeEach(() => {
    globalThis.fetch = vi.fn();
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('GET parses JSON and sends no body', async () => {
    fetch.mockResolvedValue(res(200, { value: [1, 2] }));
    const out = await api.get('/admin/api/users');
    expect(out).toEqual({ value: [1, 2] });
    const [url, opts] = fetch.mock.calls[0];
    expect(url).toBe('/admin/api/users');
    expect(opts.method).toBe('GET');
    expect(opts.body).toBeUndefined();
    expect(opts.headers).toEqual({}); // no content-type without a body
  });

  it('POST sends a JSON body with content-type', async () => {
    fetch.mockResolvedValue(res(201, { id: 'x' }));
    await api.post('/admin/api/tenants', { displayName: 'Contoso' });
    const [url, opts] = fetch.mock.calls[0];
    expect(url).toBe('/admin/api/tenants');
    expect(opts.method).toBe('POST');
    expect(opts.headers['Content-Type']).toBe('application/json');
    expect(JSON.parse(opts.body)).toEqual({ displayName: 'Contoso' });
  });

  it('PATCH and DELETE use the right verbs', async () => {
    fetch.mockResolvedValue(res(200, {}));
    await api.patch('/x', { a: 1 });
    expect(fetch.mock.calls[0][1].method).toBe('PATCH');
    fetch.mockResolvedValue(res(204, null));
    await api.del('/x/1');
    expect(fetch.mock.calls[1][1].method).toBe('DELETE');
  });

  it('204 No Content resolves to null without parsing', async () => {
    const r = res(204, undefined);
    r.json = vi.fn(); // must not be called
    fetch.mockResolvedValue(r);
    await expect(api.del('/x/1')).resolves.toBeNull();
    expect(r.json).not.toHaveBeenCalled();
  });

  it('extracts error.message from the admin error envelope', async () => {
    fetch.mockResolvedValue(res(409, { error: { code: 'conflict', message: 'Already exists.' } }));
    await expect(api.post('/x', {})).rejects.toThrow('Already exists.');
  });

  it('falls back to error.code, then to HTTP status', async () => {
    fetch.mockResolvedValue(res(400, { error: { code: 'validation_error' } }));
    await expect(api.get('/x')).rejects.toThrow('validation_error');

    fetch.mockResolvedValue({ status: 500, ok: false, json: () => Promise.reject(new Error('no json')) });
    await expect(api.get('/x')).rejects.toThrow('HTTP 500');
  });

  it('copy() writes to the clipboard when available', () => {
    const writeText = vi.fn();
    vi.stubGlobal('navigator', { clipboard: { writeText } });
    copy('secret-token');
    expect(writeText).toHaveBeenCalledWith('secret-token');
    vi.unstubAllGlobals();
  });

  it('copy() is a no-op when clipboard is unavailable', () => {
    vi.stubGlobal('navigator', {});
    expect(() => copy('x')).not.toThrow();
    vi.unstubAllGlobals();
  });
});
