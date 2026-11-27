import { expect } from '@playwright/test';
import { loadEnv } from 'vite';
import { test } from './fixtures/e2e-fixtures';

const ICP_URL = 'https://beian.miit.gov.cn/';
const MOBILE_VIEWPORT = { width: 390, height: 844 };

const FILING = (
  loadEnv('development', process.cwd(), 'VITE_').VITE_ICP_BEIAN ?? ''
).trim();

test('e2e stack carries an ICP filing number', () => {
  test.skip(!process.env.CI, 'local stacks may run without a filing number');
  expect(FILING).not.toBe('');
});

test.describe('ICP filing footer', () => {
  test.skip(!FILING, 'VITE_ICP_BEIAN is not set for this stack');

  test('shows the filing number above the login gate and links to MIIT', async ({
    page,
    context,
  }) => {
    await context.route(
      (url) => url.hostname === 'beian.miit.gov.cn',
      (route) =>
        route.fulfill({
          status: 200,
          contentType: 'text/html',
          body: '<html><body>MIIT</body></html>',
        }),
    );

    await page.goto('/');
    await expect(page.getByRole('dialog')).toBeVisible();

    const link = page.getByTestId('icp-beian-link');
    await expect(link).toBeVisible();
    await expect(link).toHaveText(FILING);
    await expect(link).toHaveAttribute('href', ICP_URL);
    await expect(link).toHaveAttribute('target', '_blank');
    expect(((await link.getAttribute('rel')) ?? '').split(/\s+/)).toEqual(
      expect.arrayContaining(['noopener', 'noreferrer']),
    );

    const uncovered = await link.evaluate((el) => {
      const rect = el.getBoundingClientRect();
      const hit = document.elementFromPoint(
        rect.left + rect.width / 2,
        rect.top + rect.height / 2,
      );
      return hit === el || el.contains(hit);
    });
    expect(uncovered).toBe(true);

    const popupPromise = page.waitForEvent('popup');
    await link.click();
    const popup = await popupPromise;
    await popup.waitForURL(ICP_URL);
    await popup.close();
  });

  test('keeps the filing number visible on a mobile viewport', async ({
    page,
  }) => {
    await page.setViewportSize(MOBILE_VIEWPORT);
    await page.goto('/');

    const link = page.getByTestId('icp-beian-link');
    await expect(link).toBeVisible();
    await expect(link).toHaveText(FILING);
    const box = await link.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.y + box!.height).toBeLessThanOrEqual(MOBILE_VIEWPORT.height);
  });
});
