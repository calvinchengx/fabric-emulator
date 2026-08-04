// Hash routing, one level deep: `#view` and `#view/param`.
//
// WHY A PARAM AT ALL. Every page here was a leaf, so a detail view had to be an
// accordion inside its list — and an accordion cannot be linked to. The flow
// graph already knows which node is which semantic model and lights it up when
// Power BI queries it; without an address for "that model" it is a dead end.
// Nothing could point at anything.
//
// ONE LEVEL, deliberately. `#models/{id}` is the whole requirement; a general
// path router would be more code for a second segment nobody has asked for.
// When `#workspaces/{id}/items/{id}` turns up, that is the moment to grow this
// — not before, and it is a small function to grow.
//
// The param is decoded, because item ids are GUIDs today and display names are
// the obvious next thing someone links to.

/** Parse a location hash into the view and its optional parameter. */
export function parse(hash) {
  const raw = (hash || '').replace(/^#/, '');
  if (!raw) return { view: 'dashboard', param: null };
  const cut = raw.indexOf('/');
  if (cut === -1) return { view: raw, param: null };
  const param = raw.slice(cut + 1);
  return {
    view: raw.slice(0, cut),
    // An empty param — `#models/` — is not a detail request. Treating it as one
    // would render a detail page for the id `""` and report it missing, which
    // reads as a broken link rather than a stray slash.
    param: param === '' ? null : safeDecode(param),
  };
}

function safeDecode(s) {
  try {
    return decodeURIComponent(s);
  } catch {
    // A half-typed or mangled escape is a bad address, not a crash. The raw
    // text still identifies nothing, and the page will say so.
    return s;
  }
}

/** The href for a view, with an optional parameter. Encode once, here. */
export function href(view, param) {
  return param == null || param === ''
    ? `#${view}`
    : `#${view}/${encodeURIComponent(param)}`;
}

/** Subscribe to hash changes; returns an unsubscribe. */
export function onRouteChange(fn) {
  const handler = () => fn(parse(location.hash));
  window.addEventListener('hashchange', handler);
  return () => window.removeEventListener('hashchange', handler);
}
