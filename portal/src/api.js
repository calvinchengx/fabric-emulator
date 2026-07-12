// Thin client for the admin REST API (same origin as the portal).
async function call(method, path, body) {
  const resp = await fetch(path, {
    method,
    headers: body !== undefined ? { 'Content-Type': 'application/json' } : {},
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (resp.status === 204) return null;
  const data = await resp.json().catch(() => null);
  if (!resp.ok) {
    const msg = data?.error?.message || data?.error?.code || `HTTP ${resp.status}`;
    throw new Error(msg);
  }
  return data;
}

export const api = {
  get: (path) => call('GET', path),
  post: (path, body) => call('POST', path, body),
  patch: (path, body) => call('PATCH', path, body),
  del: (path) => call('DELETE', path),
};

export function copy(text) {
  navigator.clipboard?.writeText(text);
}
