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

import { DEFAULT_LOCALE, i18n, t } from '~/i18n';

const MS_PER_SECOND = 1_000;
const MS_PER_MINUTE = 60 * MS_PER_SECOND;
const MS_PER_HOUR = 60 * MS_PER_MINUTE;
const MS_PER_DAY = 24 * MS_PER_HOUR;
const MS_PER_WEEK = 7 * MS_PER_DAY;

const rtfCache = new Map<string, Intl.RelativeTimeFormat>();
const dtfCache = new Map<string, Intl.DateTimeFormat>();

function locale(): string {
  return i18n.language || DEFAULT_LOCALE;
}

function relativeFormatter(): Intl.RelativeTimeFormat {
  const key = locale();
  let formatter = rtfCache.get(key);
  if (!formatter) {
    formatter = new Intl.RelativeTimeFormat(key, { numeric: 'auto' });
    rtfCache.set(key, formatter);
  }
  return formatter;
}

function absoluteFormatter(): Intl.DateTimeFormat {
  const key = locale();
  let formatter = dtfCache.get(key);
  if (!formatter) {
    formatter = new Intl.DateTimeFormat(key, {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
    });
    dtfCache.set(key, formatter);
  }
  return formatter;
}

export function formatRelativeTime(input: string | Date, now: Date = new Date()): string {
  const date = typeof input === 'string' ? new Date(input) : input;
  const diffMs = date.getTime() - now.getTime();
  const absMs = Math.abs(diffMs);

  if (absMs < MS_PER_MINUTE) return t('time.justNow');
  if (absMs < MS_PER_HOUR) {
    return relativeFormatter().format(Math.round(diffMs / MS_PER_MINUTE), 'minute');
  }
  if (absMs < MS_PER_DAY) {
    return relativeFormatter().format(Math.round(diffMs / MS_PER_HOUR), 'hour');
  }
  if (absMs < MS_PER_WEEK) {
    return relativeFormatter().format(Math.round(diffMs / MS_PER_DAY), 'day');
  }
  return absoluteFormatter().format(date);
}