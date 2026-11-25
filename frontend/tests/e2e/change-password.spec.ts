import { expect } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import {
  API_URL,
  closeChatPanel,
  makeTestUser,
  registerViaUi,
  seedLlmConfig,
} from './helpers/llm-config';

test.describe('Change password', () => {
  test('changes the password from the user menu and requires a fresh login', async ({
    page,
  }) => {
    const user = makeTestUser('pwchange');
    const newPassword = 'newpassword456';

    await registerViaUi(page, user);
    await seedLlmConfig(page);
    await page.reload();
    await closeChatPanel(page);

    const refreshToken = await page.evaluate(() => {
      const raw =
        window.localStorage.getItem('nyx-token-store') ??
        window.sessionStorage.getItem('nyx-token-store');
      return raw ? (JSON.parse(raw) as { refreshToken: string }).refreshToken : null;
    });
    if (!refreshToken) throw new Error('missing refresh token before password change');

    await page.getByTestId('user-menu-button').click();
    await expect(
      page.getByRole('menuitem', { name: /Change password/i }),
    ).toBeVisible();
    await page.getByTestId('user-menu-change-password').click();
    await expect(
      page.getByRole('heading', { name: /Change password/i }),
    ).toBeVisible();

    await page.getByLabel(/^Current password$/i).fill('definitely-wrong');
    await page.getByLabel(/^New password$/i).fill(newPassword);
    await page.getByLabel(/^Confirm new password$/i).fill(newPassword);
    await page.getByTestId('change-password-submit').click();
    await expect(page.getByTestId('change-password-error')).toHaveText(
      'Current password is incorrect',
    );
    await expect(page.getByTestId('current-password-input')).toBeVisible();

    await page.getByLabel(/^Current password$/i).fill(user.password);
    await page.getByTestId('change-password-submit').click();
    await expect(
      page.getByText(/Password changed\. Please sign in again\./i),
    ).toBeVisible();
    await expect(
      page.getByRole('heading', { name: 'Login or Register' }),
    ).toBeVisible();

    const revokedRefresh = await page.request.post(`${API_URL}/refresh`, {
      data: { refresh_token: refreshToken },
    });
    expect(revokedRefresh.status()).toBe(401);

    await page.getByLabel(/Username/i).fill(user.username);
    await page.getByLabel(/Password/i).fill(user.password);
    await page.getByRole('button', { name: /^Login$/i }).click();
    await expect(page.getByText(/Invalid username or password/i)).toBeVisible();

    await page.getByLabel(/Password/i).fill(newPassword);
    await page.getByRole('button', { name: /^Login$/i }).click();
    await expect(page.getByText(/Successfully logged in!/i)).toBeVisible();
    await expect(page.getByTestId('user-menu-button')).toBeVisible();
  });
});
