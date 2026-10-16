import { expect } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';

test.describe('Authentication', () => {
  const testUser = {
    username: `user_${Math.floor(Math.random() * 10000)}`,
    password: 'password123',
  };

  test('should register a new user', async ({ page }) => {
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
});
