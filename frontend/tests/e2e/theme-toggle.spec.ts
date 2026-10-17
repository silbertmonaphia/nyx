import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';

const STORAGE_KEY = 'nyx.theme';

const storedChoice = (page: Page) =>
  page.evaluate((key) => window.localStorage.getItem(key), STORAGE_KEY);

const darkClassOn = (page: Page) =>
  page.evaluate(() => document.documentElement.classList.contains('dark'));

const computedColorScheme = (page: Page) =>
  page.evaluate(() => getComputedStyle(document.documentElement).colorScheme);

const osIsDark = (page: Page) =>
  page.waitForFunction(
    () => window.matchMedia('(prefers-color-scheme: dark)').matches,
  );

const osIsLight = (page: Page) =>
  page.waitForFunction(
    () => !window.matchMedia('(prefers-color-scheme: dark)').matches,
  );

const expectDark = (page: Page) =>
  expect.poll(() => darkClassOn(page)).toBe(true);

const expectLight = (page: Page) =>
  expect.poll(() => darkClassOn(page)).toBe(false);

async function openApp(page: Page): Promise<void> {
  await page.goto('/');
  await expect(page.getByTestId('theme-toggle')).toBeVisible();
}

test.describe('Theme toggle', () => {
  test('first visit follows a dark OS scheme', async ({ page }) => {
    await page.emulateMedia({ colorScheme: 'dark' });
    await openApp(page);

    await expectDark(page);
    await expect(page.getByTestId('theme-toggle')).toHaveAttribute(
      'aria-label',
      'Switch to light theme',
    );
    expect(await computedColorScheme(page)).toBe('dark');
    expect(await storedChoice(page)).toBeNull();
  });

  test('first visit follows a light OS scheme', async ({ page }) => {
    await page.emulateMedia({ colorScheme: 'light' });
    await openApp(page);

    await expectLight(page);
    await expect(page.getByTestId('theme-toggle')).toHaveAttribute(
      'aria-label',
      'Switch to dark theme',
    );
    expect(await computedColorScheme(page)).toBe('light');
  });

  test('toggling flips the theme, persists it, and survives a reload', async ({
    page,
  }) => {
    await page.emulateMedia({ colorScheme: 'light' });
    await openApp(page);
    const toggle = page.getByTestId('theme-toggle');

    await toggle.click();

    await expectDark(page);
    await expect(toggle).toHaveAttribute('aria-label', 'Switch to light theme');
    expect(await storedChoice(page)).toBe('dark');
    expect(await computedColorScheme(page)).toBe('dark');

    await page.reload();

    await expectDark(page);
    await expect(page.getByTestId('theme-toggle')).toHaveAttribute(
      'aria-label',
      'Switch to light theme',
    );

    await page.getByTestId('theme-toggle').click();
    await expectLight(page);
    expect(await storedChoice(page)).toBe('light');
  });

  test('a stored choice reaches the document before the app script runs', async ({
    page,
  }) => {
    await page.addInitScript((key) => {
      window.localStorage.setItem(key, 'dark');
    }, STORAGE_KEY);
    await page.route('**/src/main.*', (route) => route.abort());

    await page.goto('/', { waitUntil: 'commit' });

    await expectDark(page);
    expect(await computedColorScheme(page)).toBe('dark');
  });

  test('an OS change is honored while no choice is stored', async ({ page }) => {
    await page.emulateMedia({ colorScheme: 'light' });
    await openApp(page);
    const toggle = page.getByTestId('theme-toggle');
    await expectLight(page);

    await page.emulateMedia({ colorScheme: 'dark' });
    await osIsDark(page);

    await expectDark(page);
    await expect(toggle).toHaveAttribute('aria-label', 'Switch to light theme');

    await page.emulateMedia({ colorScheme: 'light' });
    await expectLight(page);
    await expect(toggle).toHaveAttribute('aria-label', 'Switch to dark theme');
    expect(await storedChoice(page)).toBeNull();
  });

  test('a stored choice outranks a later OS change', async ({ page }) => {
    await page.emulateMedia({ colorScheme: 'light' });
    await openApp(page);
    const toggle = page.getByTestId('theme-toggle');
    await expectLight(page);

    await toggle.click();
    await expectDark(page);
    expect(await storedChoice(page)).toBe('dark');

    await page.emulateMedia({ colorScheme: 'dark' });
    await osIsDark(page);
    await page.waitForTimeout(150);
    expect(await darkClassOn(page)).toBe(true);
    await expect(toggle).toHaveAttribute('aria-label', 'Switch to light theme');

    await page.emulateMedia({ colorScheme: 'light' });
    await osIsLight(page);
    await page.waitForTimeout(150);
    expect(await darkClassOn(page)).toBe(true);
    await expect(toggle).toHaveAttribute('aria-label', 'Switch to light theme');
    expect(await computedColorScheme(page)).toBe('dark');
    expect(await storedChoice(page)).toBe('dark');
  });

  test('an in-session choice survives an OS flip when storage writes fail', async ({
    page,
  }) => {
    await page.addInitScript(() => {
      Storage.prototype.setItem = () => {
        throw new Error('QuotaExceededError');
      };
    });
    await page.emulateMedia({ colorScheme: 'light' });
    await openApp(page);
    const toggle = page.getByTestId('theme-toggle');

    await toggle.click();
    await expectDark(page);
    await expect(toggle).toHaveAttribute('aria-label', 'Switch to light theme');
    expect(await storedChoice(page)).toBeNull();

    await page.emulateMedia({ colorScheme: 'dark' });
    await osIsDark(page);
    await page.waitForTimeout(150);
    expect(await darkClassOn(page)).toBe(true);

    await page.emulateMedia({ colorScheme: 'light' });
    await osIsLight(page);
    await page.waitForTimeout(150);
    expect(await darkClassOn(page)).toBe(true);
    await expect(toggle).toHaveAttribute('aria-label', 'Switch to light theme');
    expect(await computedColorScheme(page)).toBe('dark');
  });

  test('the navbar background follows the theme', async ({ page }) => {
    await page.emulateMedia({ colorScheme: 'light' });
    await openApp(page);

    const navBg = () =>
      page.locator('nav').evaluate((el) => getComputedStyle(el).backgroundColor);
    const luminance = (rgb: string) => {
      const [r, g, b] = rgb.match(/\d+/g)!.map(Number);
      return 0.2126 * r + 0.7152 * g + 0.0722 * b;
    };

    expect(luminance(await navBg())).toBeGreaterThan(200);

    await page.getByTestId('theme-toggle').click();
    await expectDark(page);
    await expect.poll(async () => luminance(await navBg())).toBeLessThan(50);
  });

  test('the toggle is hidden below the md breakpoint', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto('/');

    await expect(page.getByTestId('theme-toggle')).toHaveCount(1);
    await expect(page.getByTestId('theme-toggle')).toBeHidden();
    await expect(page.getByText('Nyx').first()).toBeVisible();
  });
});
