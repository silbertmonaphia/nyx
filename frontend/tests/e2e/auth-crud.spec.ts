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
    await page.getByLabel(/Username/i).fill(testUser.username);
    await page.getByLabel(/Password/i).fill(testUser.password);

    // Submit
    await page.getByRole('button', { name: /^Register$/i }).click();

    // Verify toast and login state
    await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
    // The header shows the username as plain text and a Logout button —
    // both gate on `isAuthenticated`, so seeing them is proof the
    // session landed.
    await expect(page.getByText(testUser.username)).toBeVisible();
    await expect(page.getByRole('button', { name: /Logout/i })).toBeVisible();
  });

  test('should perform CRUD operations on feeds', async ({ page }) => {
    // Each test gets a fresh browser context, so the random
    // `testUser.username` from the describe scope does not exist on
    // the backend yet. Register first, then log out + log in to
    // exercise the login path explicitly. Without the register step
    // the login would 401 silently and the subsequent CRUD checks
    // would time out waiting for the auth-gated "Add feed" button.

    await page.goto('/');
    await page.getByRole('button', { name: /Login \/ Register/i }).click();
    // Switch to Register
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

    // 4. Delete — uses the new Radix-based confirm dialog, not window.confirm
    await page.getByTitle('Delete').first().click();
    await page.getByTestId('confirm-delete').click();

    await expect(page.getByText(updatedTitle)).not.toBeVisible();
  });
});