import { expect } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';

const API_URL = process.env.E2E_API_URL ?? 'http://localhost:8080/api';

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
    const toastBox = await page.getByTestId('toast').boundingBox();
    const viewport = page.viewportSize();
    expect(toastBox).not.toBeNull();
    expect(viewport).not.toBeNull();
    expect(
      Math.abs(toastBox!.x + toastBox!.width / 2 - viewport!.width / 2),
    ).toBeLessThan(2);
    // The header shows the username as plain text and a Logout button —
    // both gate on `isAuthenticated`, so seeing them is proof the
    // session landed.
    await expect(page.getByText(testUser.username)).toBeVisible();
    await expect(page.getByRole('button', { name: /Logout/i })).toBeVisible();
  });

  test('should keep the session in a new window when remember me is checked', async ({
    page,
    context,
  }) => {
    const username = `remember_${Date.now()}_${Math.floor(Math.random() * 10000)}`;

    await page.goto('/');
    await page.getByRole('button', { name: /Login \/ Register/i }).click();
    await page.getByRole('button', { name: /Register/i }).last().click();
    await page.getByLabel(/Username/i).fill(username);
    await page.getByLabel(/Password/i).fill('password123');
    await expect(page.getByLabel(/Remember me/i)).toBeChecked();
    await page.getByRole('button', { name: /^Register$/i }).click();
    await expect(page.getByRole('button', { name: /^Logout$/i })).toBeVisible();
    expect(
      await page.evaluate(() => window.localStorage.getItem('nyx-token-store')),
    ).not.toBeNull();

    await page.close();
    const reopened = await context.newPage();
    await reopened.goto('/');

    await expect(reopened.getByRole('button', { name: /^Logout$/i })).toBeVisible();
    await expect(reopened.getByText(username)).toBeVisible();
  });

  test('should revoke the session on normal logout', async ({ page }) => {
    const username = `logout_${Date.now()}_${Math.floor(Math.random() * 10000)}`;

    await page.goto('/');
    await page.getByRole('button', { name: /Login \/ Register/i }).click();
    await page.getByRole('button', { name: /Register/i }).last().click();
    await page.getByLabel(/Username/i).fill(username);
    await page.getByLabel(/Password/i).fill('password123');
    await page.getByRole('button', { name: /^Register$/i }).click();
    await expect(page.getByRole('button', { name: /^Logout$/i })).toBeVisible();

    const refreshToken = await page.evaluate(() => {
      const raw =
        window.localStorage.getItem('nyx-token-store') ??
        window.sessionStorage.getItem('nyx-token-store');
      return raw ? (JSON.parse(raw) as { refreshToken: string }).refreshToken : null;
    });
    expect(refreshToken).not.toBeNull();

    const logoutPosted = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname.endsWith('/api/logout') &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: /^Logout$/i }).click();
    expect((await logoutPosted).status()).toBe(204);

    await expect(
      page.getByRole('button', { name: /Login \/ Register/i }),
    ).toBeVisible();

    const refreshed = await page.request.post(`${API_URL}/refresh`, {
      data: { refresh_token: refreshToken },
    });
    expect(refreshed.status()).toBe(401);
  });


  test('wrong password shows an inline error instead of a session-expired toast', async ({
    page,
  }) => {
    const user = {
      username: `wrongpass_${Date.now()}_${Math.floor(Math.random() * 10000)}`,
      password: 'password123',
    };

    await page.goto('/');
    await page.getByRole('button', { name: /Login \/ Register/i }).click();
    await page.getByRole('button', { name: /Register/i }).last().click();
    await page.getByLabel(/Username/i).fill(user.username);
    await page.getByLabel(/Password/i).fill(user.password);
    await page.getByRole('button', { name: /^Register$/i }).click();
    await expect(page.getByRole('button', { name: /^Logout$/i })).toBeVisible();

    await page.getByRole('button', { name: /^Logout$/i }).click();
    await page.getByRole('button', { name: /Login \/ Register/i }).click();
    await page.getByLabel(/Username/i).fill(user.username);
    await page.getByLabel(/Password/i).fill('wrongpassword');
    await page.getByRole('button', { name: /^Login$/i }).click();

    const alert = page.getByRole('alert');
    await expect(alert).toHaveText(/Invalid username or password/i);
    await expect(page.getByText(/Session expired/i)).toHaveCount(0);

    const inputBox = await page.getByLabel(/Password/i).boundingBox();
    const alertBox = await alert.boundingBox();
    expect(inputBox).not.toBeNull();
    expect(alertBox).not.toBeNull();
    expect(alertBox!.y).toBeGreaterThan(inputBox!.y);
  });
});
