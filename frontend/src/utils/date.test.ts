import { describe, it, expect } from 'vitest';
import { formatRelativeTime } from './date';

const NOW = new Date('2026-09-19T12:00:00Z');

describe('formatRelativeTime', () => {
  it('returns "just now" for less than a minute ago', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 5_000), NOW)).toBe('just now');
    expect(formatRelativeTime(new Date(NOW.getTime() + 30_000), NOW)).toBe('just now');
  });

  it('formats minutes ago / in the future', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 5 * 60_000), NOW)).toBe('5 minutes ago');
    expect(formatRelativeTime(new Date(NOW.getTime() + 5 * 60_000), NOW)).toBe('in 5 minutes');
  });

  it('formats hours ago / in the future', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 3 * 3_600_000), NOW)).toBe('3 hours ago');
    expect(formatRelativeTime(new Date(NOW.getTime() + 2 * 3_600_000), NOW)).toBe('in 2 hours');
  });

  it('formats days ago / in the future', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 2 * 86_400_000), NOW)).toBe('2 days ago');
    expect(formatRelativeTime(new Date(NOW.getTime() + 3 * 86_400_000), NOW)).toBe('in 3 days');
  });

  it('falls back to an absolute date for intervals beyond a week', () => {
    const farPast = new Date(NOW.getTime() - 30 * 86_400_000);
    const out = formatRelativeTime(farPast, NOW);
    // Locale-dependent — assert it is NOT a relative phrase.
    expect(out).not.toMatch(/ago|in \d+/);
    // And contains the year.
    expect(out).toMatch(/2025|2026/);
  });

  it('accepts an ISO string', () => {
    expect(formatRelativeTime(new Date(NOW.getTime() - 90_000).toISOString(), NOW)).toBe(
      '1 minute ago',
    );
  });
});