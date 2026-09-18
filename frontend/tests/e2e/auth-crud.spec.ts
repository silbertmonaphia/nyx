import { test, expect } from '@playwright/test';

test.describe('Authentication and Feed CRUD', () => {
  const testUser = {
    username: `user_${Math.floor(Math.random() * 10000)}`,
    password: 'password123',
  };

  test('should register a new user and login', async ({ page }) => {
    await page.goto('/');

    // Open Auth Form
    await page.getByRole('button', { name: /Login \/ Register/i }).click();

    // Switch to Register
    await page.getByRole('button', { name: /Register/i }).last().click();

    // Fill form
    await page.getByPlaceholder(/Your username/i).fill(testUser.username);
    await page.getByPlaceholder(/••••••••/i).fill(testUser.password);

    // Submit
    await page.getByRole('button', { name: /^Register$/i }).click();

    // Verify toast and login state
    await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
    await expect(page.getByText(`Welcome, ${testUser.username}`)).toBeVisible();
  });

  test('should perform CRUD operations on feeds', async ({ page }) => {
    // We assume the user from previous test is still logged in if we run them together,
    // but for isolation we'd usually login here.
    // For this prototype, let's just do a fresh login if needed or run sequentially.

    await page.goto('/');
    await page.getByRole('button', { name: /Login \/ Register/i }).click();
    await page.getByPlaceholder(/Your username/i).fill(testUser.username);
    await page.getByPlaceholder(/••••••••/i).fill(testUser.password);
    await page.getByRole('button', { name: /^Login$/i }).click();

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

    // 4. Delete — uses the new Radix-based confirm dialog, not window.confirm
    await page.getByTitle('Delete').first().click();
    await page.getByTestId('confirm-delete').click();

    await expect(page.getByText(updatedTitle)).not.toBeVisible();
  });
});