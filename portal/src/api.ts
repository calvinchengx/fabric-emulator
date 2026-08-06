// Thin client for the admin REST API (same origin as the portal).
//
// WHY THE RETURN TYPE IS `any`. Each endpoint has its own shape and none of
// them is generated, so a typed envelope here would be an unchecked claim about
// the server — the same drift that made the portal's hand-written event-kind
// list a bug. The event stream IS typed, because its contract is generated from
// internal/store/bus.go and cannot disagree with the server. Until an endpoint
// is generated too, its response stays `any` and the caller reads what it needs.
async function call(method: string, path: string, body?: unknown): Promise<any> {
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
  get: (path: string) => call('GET', path),
  post: (path: string, body?: unknown) => call('POST', path, body),
  patch: (path: string, body?: unknown) => call('PATCH', path, body),
  del: (path: string) => call('DELETE', path),
};

export function copy(text: string) {
  navigator.clipboard?.writeText(text);
}
