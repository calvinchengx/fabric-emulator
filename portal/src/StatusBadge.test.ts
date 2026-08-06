import { render, screen } from '@testing-library/svelte';
// Svelte 5 snippets are the children API; createRawSnippet is how a test
// builds one.
import { createRawSnippet } from 'svelte';
import { describe, expect, it } from 'vitest';
import StatusBadge, { toneOf } from '$lib/StatusBadge.svelte';

// The badge is how every view reports state, so a wrong tone is a wrong claim
// made quietly: a failed job painted as success reads as a passing run. The
// vocabulary is asserted directly rather than through one view that happens to
// use it.

describe('toneOf', () => {
  it.each([
    ['Completed', 'success'],
    ['Succeeded', 'success'],
    ['Active', 'success'],
    ['streaming', 'success'],
    ['Failed', 'danger'],
    ['Running', 'caution'],
    ['InProgress', 'caution'],
    ['NotStarted', 'caution'],
    ['connecting', 'caution'],
    ['reconnecting', 'caution'],
    ['Cancelled', 'muted'],
  ])('reads %s as %s', (status, tone) => {
    expect(toneOf(status)).toBe(tone);
  });

  it('is case- and separator-insensitive, because the wire and the CSS disagreed', () => {
    // The API says `NotStarted`, the old stylesheet said `notstarted`, and a
    // hand-written status might say `not started`.
    for (const spelling of ['NotStarted', 'notstarted', 'not started', 'not-started', 'NOT_STARTED']) {
      expect(toneOf(spelling), spelling).toBe('caution');
    }
  });

  it('gives a word it does not know no tone at all', () => {
    // Better than guessing: an unknown status renders neutral rather than
    // borrowing a colour that would assert something untrue.
    expect(toneOf('Paused')).toBe('');
    expect(toneOf(undefined)).toBe('');
    expect(toneOf(null)).toBe('');
    expect(toneOf('')).toBe('');
  });
});

describe('StatusBadge', () => {
  it('labels itself with the status when given no children', () => {
    render(StatusBadge, { props: { status: 'Completed' } });
    const badge = screen.getByText('Completed');
    expect(badge).toHaveAttribute('data-tone', 'success');
  });

  it('lets the view override the tone the word would carry', () => {
    // Capacities does this: any state that is not Active reads as caution,
    // whatever it happens to be called.
    render(StatusBadge, { props: { status: 'Paused', tone: 'caution' } });
    expect(screen.getByText('Paused')).toHaveAttribute('data-tone', 'caution');
  });

  it('renders children instead of the status when both are given', () => {
    render(StatusBadge, {
      props: { status: 'ignored', children: createRawSnippet(() => ({ render: () => '<span>4 dropped</span>' })) },
    });
    expect(screen.getByText('4 dropped')).toBeInTheDocument();
  });
});
