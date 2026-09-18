/**
 * Human-readable relative time formatting.
 *
 * - "just now" for < 1 minute
 * - "X minutes ago" / "in X minutes" up to an hour
 * - "X hours ago" / "in X hours" up to a day
 * - "X days ago" / "in X days" up to a week
 * - Falls back to an absolute date (e.g. "Jan 1, 2024") for older dates
 *
 * Uses `Intl.RelativeTimeFormat` / `Intl.DateTimeFormat`, so the output is
 * localized automatically and no date library is required.
 */

const MS_PER_SECOND = 1_000;
const MS_PER_MINUTE = 60 * MS_PER_SECOND;
const MS_PER_HOUR = 60 * MS_PER_MINUTE;
const MS_PER_DAY = 24 * MS_PER_HOUR;
const MS_PER_WEEK = 7 * MS_PER_DAY;

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });
const dtf = new Intl.DateTimeFormat(undefined, {
  year: 'numeric',
  month: 'short',
  day: 'numeric',
});

export function formatRelativeTime(input: string | Date, now: Date = new Date()): string {
  const date = typeof input === 'string' ? new Date(input) : input;
  const diffMs = date.getTime() - now.getTime();
  const absMs = Math.abs(diffMs);

  if (absMs < MS_PER_MINUTE) return 'just now';
  if (absMs < MS_PER_HOUR) return rtf.format(Math.round(diffMs / MS_PER_MINUTE), 'minute');
  if (absMs < MS_PER_DAY) return rtf.format(Math.round(diffMs / MS_PER_HOUR), 'hour');
  if (absMs < MS_PER_WEEK) return rtf.format(Math.round(diffMs / MS_PER_DAY), 'day');
  return dtf.format(date);
}