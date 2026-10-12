import { test, expect } from '@playwright/test';

/**
 * End-to-end coverage of the feed list CRUD flow.
 *
 * Exercises the full request → wire → render path through a real
 * Chromium so regressions in the middle of the chain (optimistic
 * update ordering, cache invalidation, search debounce, Radix
 * confirm dialog) can't pass the unit tests while breaking the
 * real UI.
 *
 * Register first because each test starts from a fresh browser
 * context; the random `testUser` per `describe` doesn't exist on
 * the backend until something registers it. Logout + login is
 * explicit so the Login path is exercised too.
 */
test.describe('Feed CRUD', () => {
  const testUser = {
    username: `feed_${Math.floor(Math.random() * 10000)}`,
    password: 'password123',
  };

  test('should perform CRUD operations on feeds', async ({ page }) => {
    // Default 30s is too tight: the flow does register → logout →
    // login → create → search → update → delete, and the update's
    // optimistic-cache invalidation can race the re-fetch on a slow
    // CI runner. 90s leaves headroom without hiding real bugs.
    test.setTimeout(90_000);

    await page.goto('/');

    // Register (testUser is random per describe; nothing exists yet)
    await page.getByRole('button', { name: /Login \/ Register/i }).click();
    await page.getByRole('button', { name: /Register/i }).last().click();
    await page.getByLabel(/Username/i).fill(testUser.username);
    await page.getByLabel(/Password/i).fill(testUser.password);
    await page.getByRole('button', { name: /^Register$/i }).click();
    await expect(page.getByRole('button', { name: /Logout/i })).toBeVisible();

    // Log out, then log back in to verify the Login path too.
    await page.getByRole('button', { name: /Logout/i }).click();
    await expect(page.getByRole('button', { name: /Logout/i })).toHaveCount(0);
    await page.getByRole('button', { name: /Login \/ Register/i }).click();
    await page.getByLabel(/Username/i).fill(testUser.username);
    await page.getByLabel(/Password/i).fill(testUser.password);
    await page.getByRole('button', { name: /^Login$/i }).click();
    await expect(page.getByRole('button', { name: /Logout/i })).toBeVisible();

    // 1. Create
    const feedTitle = `Test Feed ${Date.now()}`;
    await page.getByRole('button', { name: /Add Feed/i }).click();
    await page.getByPlaceholder(/Feed title/i).fill(feedTitle);
    await page.getByPlaceholder(/Description/i).fill('This is a test description.');
    await page.getByRole('button', { name: /Save Feed/i }).click();

    await expect(page.getByText(feedTitle)).toBeVisible();

    // 2. Read (Search)
    await page.getByPlaceholder(/Search for feeds/i).fill(feedTitle);
    await expect(page.getByText(feedTitle)).toBeVisible();

    // 3. Update
    const updatedTitle = `${feedTitle} Updated`;
    await page.getByTitle('Edit').first().click();
    await page.getByPlaceholder(/Feed title/i).fill(updatedTitle);
    await page.getByRole('button', { name: /Update Feed/i }).click();

    await expect(page.getByText(updatedTitle)).toBeVisible();

    // 4. Delete — uses the Radix-based confirm dialog, not window.confirm
    await page.getByTitle('Delete').first().click();
    await page.getByTestId('confirm-delete').click();

    await expect(page.getByText(updatedTitle)).not.toBeVisible();
  });
});
