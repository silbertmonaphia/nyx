import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { en } from './en';
import {
  DEFAULT_LOCALE,
  LOCALE_STORAGE_KEY,
  SUPPORTED_LOCALES,
  i18n,
  resolveLocale,
  setLocale,
  t,
} from './index';
import { zh } from './zh';

const leafPairs = (node: unknown, prefix = ''): [string, unknown][] => {
  if (typeof node !== 'object' || node === null) {
    return [[prefix, node]];
  }
  return Object.entries(node).flatMap(([key, value]) =>
    leafPairs(value, prefix ? `${prefix}.${key}` : key),
  );
};

const leafPaths = (node: unknown): string[] =>
  leafPairs(node).map(([path]) => path);

beforeEach(async () => {
  window.localStorage.clear();
  await i18n.changeLanguage(DEFAULT_LOCALE);
});

afterEach(async () => {
  window.localStorage.clear();
  await i18n.changeLanguage(DEFAULT_LOCALE);
});

describe('locale resolution', () => {
  it('defaults to zh with nothing stored', () => {
    expect(DEFAULT_LOCALE).toBe('zh');
    expect(SUPPORTED_LOCALES).toEqual(['zh', 'en']);
    expect(resolveLocale()).toBe('zh');
  });

  it('ignores the browser language and never falls back to it', async () => {
    expect(window.navigator.language).toMatch(/^en/i);
    expect(resolveLocale()).toBe('zh');
    expect(i18n.language).toBe('zh');
    expect(t('common.cancel')).toBe('取消');
  });

  it('reads a remembered choice', () => {
    window.localStorage.setItem(LOCALE_STORAGE_KEY, 'en');
    expect(resolveLocale()).toBe('en');
  });

  it('ignores an unsupported stored value', () => {
    window.localStorage.setItem(LOCALE_STORAGE_KEY, 'fr');
    expect(resolveLocale()).toBe('zh');
  });

  it('degrades gracefully when storage is unavailable', () => {
    const getItem = Storage.prototype.getItem;
    Storage.prototype.getItem = () => {
      throw new Error('SecurityError');
    };
    try {
      expect(resolveLocale()).toBe('zh');
    } finally {
      Storage.prototype.getItem = getItem;
    }
  });
});

describe('language switching', () => {
  it('applies to t() immediately', async () => {
    await i18n.changeLanguage('en');
    expect(t('common.cancel')).toBe('Cancel');
    expect(t('nav.loginOrRegister')).toBe('Login / Register');

    await i18n.changeLanguage('zh');
    expect(t('common.cancel')).toBe('取消');
    expect(t('nav.loginOrRegister')).toBe('登录 / 注册');
  });

  it('remembers the choice and survives reload', async () => {
    setLocale('en');
    await i18n.changeLanguage('en');
    expect(window.localStorage.getItem(LOCALE_STORAGE_KEY)).toBe('en');
    expect(resolveLocale()).toBe('en');

    setLocale('zh');
    await i18n.changeLanguage('zh');
    expect(window.localStorage.getItem(LOCALE_STORAGE_KEY)).toBe('zh');
    expect(resolveLocale()).toBe('zh');
  });

  it('keeps the choice in memory when the write fails', async () => {
    const setItem = Storage.prototype.setItem;
    Storage.prototype.setItem = () => {
      throw new Error('QuotaExceededError');
    };
    try {
      await i18n.changeLanguage('en');
    } finally {
      Storage.prototype.setItem = setItem;
    }
    expect(t('common.cancel')).toBe('Cancel');
    expect(i18n.language).toBe('en');
  });

  it('syncs the document language', async () => {
    await i18n.changeLanguage('en');
    expect(document.documentElement.lang).toBe('en');

    await i18n.changeLanguage('zh');
    expect(document.documentElement.lang).toBe('zh');
  });
});

describe('interpolation', () => {
  it('interpolates named values per locale', async () => {
    await i18n.changeLanguage('en');
    expect(t('feeds.countOfTotal', { loaded: 2, total: 42 })).toBe(
      '2 of 42 feeds loaded',
    );
    expect(t('feeds.emptySearch', { term: 'matrix' })).toBe(
      'Feed "matrix" not found',
    );

    await i18n.changeLanguage('zh');
    expect(t('feeds.countOfTotal', { loaded: 2, total: 42 })).toBe(
      '已加载 2 / 42 个知识碎片',
    );
    expect(t('feeds.emptySearch', { term: 'matrix' })).toBe(
      '未找到知识碎片 "matrix"',
    );
  });

  it('does not escape interpolated values', async () => {
    await i18n.changeLanguage('en');
    expect(t('feeds.emptySearch', { term: 'a & <b>' })).toBe(
      'Feed "a & <b>" not found',
    );
  });

  it('selects plurals from count', async () => {
    await i18n.changeLanguage('en');
    expect(t('chat.steps', { count: 1 })).toBe('1 step');
    expect(t('chat.steps', { count: 3 })).toBe('3 steps');
    expect(t('feeds.countLoaded', { count: 1 })).toBe('1 feed loaded');
    expect(t('feeds.countLoaded', { count: 2 })).toBe('2 feeds loaded');
    expect(t('time.daysAgo', { count: 1 })).toBe('1 day ago');
    expect(t('time.daysAgo', { count: 3 })).toBe('3 days ago');
    expect(t('chat.retryTitle', { count: 2 })).toBe(
      'succeeded after 2 retries',
    );

    await i18n.changeLanguage('zh');
    expect(t('chat.steps', { count: 1 })).toBe('1 个步骤');
    expect(t('chat.steps', { count: 3 })).toBe('3 个步骤');
    expect(t('time.daysAgo', { count: 3 })).toBe('3 天前');
    expect(t('chat.retryTitle', { count: 2 })).toBe('重试 2 次后成功');
  });
});

describe('resource parity', () => {
  it('carries the same key set in both locales', () => {
    expect(leafPaths(en)).toEqual(leafPaths(zh));
  });

  it('covers every surface', () => {
    expect(Object.keys(zh.translation)).toEqual([
      'app',
      'nav',
      'locale',
      'theme',
      'auth',
      'feeds',
      'chat',
      'llmConfig',
      'time',
      'errors',
      'common',
    ]);
  });

  it('has no empty strings', () => {
    for (const [path, value] of leafPairs(en)) {
      expect(typeof value, path).toBe('string');
      expect((value as string).trim(), path).not.toBe('');
    }
    for (const [path, value] of leafPairs(zh)) {
      expect(typeof value, path).toBe('string');
      expect((value as string).trim(), path).not.toBe('');
    }
  });

  it('keeps placeholder names aligned across locales', () => {
    const placeholders = (value: string) =>
      [...value.matchAll(/\{\{(\w+)\}\}/g)].map((m) => m[1]).sort();
    const enPairs = new Map(leafPairs(en));
    for (const [path, value] of leafPairs(zh)) {
      expect(placeholders(enPairs.get(path) as string), path).toEqual(
        placeholders(value as string),
      );
    }
  });
});
