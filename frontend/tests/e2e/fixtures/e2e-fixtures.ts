import { test as base } from '@playwright/test';

export const LOCALE_STORAGE_KEY = 'nyx.locale';
export const PINNED_LOCALE = 'en';

export const test = base.extend({
  context: async ({ context }, use) => {
    await context.addInitScript((entry) => {
      try {
        window.localStorage.setItem(entry.key, entry.value);
      } catch {
        return;
      }
    }, { key: LOCALE_STORAGE_KEY, value: PINNED_LOCALE });
    // eslint-disable-next-line react-hooks/rules-of-hooks
    await use(context);
  },
});
