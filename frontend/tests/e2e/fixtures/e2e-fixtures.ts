import { test as base } from '@playwright/test';

import { LOCALE_STORAGE_KEY } from '../../../src/i18n/locale-storage';

export const PINNED_LOCALE = 'en';

export const test = base.extend({
  context: async ({ context }, provide) => {
    await context.addInitScript((entry) => {
      try {
        window.localStorage.setItem(entry.key, entry.value);
      } catch {
        return;
      }
    }, { key: LOCALE_STORAGE_KEY, value: PINNED_LOCALE });
    await provide(context);
  },
});
