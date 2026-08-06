import { describe, expect, it, vi } from 'vitest';
import { href, onRouteChange, parse } from './router.js';

describe('router', () => {
  it.each([
    ['', 'dashboard', null],
    ['#', 'dashboard', null],
    ['#models', 'models', null],
    ['#models/abc-123', 'models', 'abc-123'],
    // A trailing slash is a stray keystroke, not a request for the detail page
    // of the item named "". Treated as one, the page reports "not found" for an
    // id nobody typed, which reads as a broken link.
    ['#models/', 'models', null],
    // The param may itself contain slashes once something addresses a path.
    // Keeping the remainder whole means this does not have to change then.
    ['#models/a/b', 'models', 'a/b'],
  ])('parses %o', (hash, view, param) => {
    expect(parse(hash)).toEqual({ view, param });
  });

  it('decodes the parameter, because ids are not always GUIDs', () => {
    expect(parse('#models/' + encodeURIComponent('My Model'))).toEqual({
      view: 'models',
      param: 'My Model',
    });
  });

  it('survives a mangled escape instead of throwing', () => {
    // decodeURIComponent('%') throws. A half-typed address is a bad link, and
    // the page should say it found nothing — not white-screen the portal.
    expect(() => parse('#models/%')).not.toThrow();
    expect(parse('#models/%').view).toBe('models');
  });

  it('builds hrefs that round-trip through parse', () => {
    for (const [view, param] of [
      ['models', null],
      ['models', 'abc-123'],
      ['models', 'My Model'],
      ['models', 'a/b'],
    ]) {
      expect(parse(href(view, param))).toEqual({ view, param });
    }
  });

  it('omits the separator when there is no parameter', () => {
    // `#models/` would otherwise be produced and then parsed back to a null
    // param — correct, but it puts a meaningless slash in every nav link.
    expect(href('models')).toBe('#models');
    expect(href('models', null)).toBe('#models');
    expect(href('models', '')).toBe('#models');
  });

  describe('onRouteChange', () => {
    it('reports the parsed route on every hash change', () => {
      const seen = vi.fn();
      const stop = onRouteChange(seen);
      try {
        location.hash = '#models/abc-123';
        window.dispatchEvent(new Event('hashchange'));
        expect(seen).toHaveBeenCalledWith({ view: 'models', param: 'abc-123' });

        // A second change must also arrive: a subscriber that fires once would
        // leave the portal showing the page you navigated away from.
        location.hash = '#flow';
        window.dispatchEvent(new Event('hashchange'));
        expect(seen).toHaveBeenLastCalledWith({ view: 'flow', param: null });
        expect(seen).toHaveBeenCalledTimes(2);
      } finally {
        stop();
      }
    });

    it('stops listening when unsubscribed', () => {
      // The returned function is what App's teardown relies on. If it did not
      // detach, every remount would add another listener and one hash change
      // would set the route N times.
      const seen = vi.fn();
      onRouteChange(seen)();
      location.hash = '#jobs';
      window.dispatchEvent(new Event('hashchange'));
      expect(seen).not.toHaveBeenCalled();
    });
  });
});
