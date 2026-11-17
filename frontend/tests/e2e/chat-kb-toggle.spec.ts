import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import {
  API_URL,
  readAccessToken,
  registerAndOpenChat,
} from './helpers/llm-config';

async function reloadWithFeatures(
  page: Page,
  agentEnabled: boolean,
): Promise<void> {
  await page.route('**/api/features', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ agent_enabled: agentEnabled }),
    });
  });
  await page.reload();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

test.describe('Knowledge toggle capability', () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page, { prefix: 'e2e_kb_toggle' });
  });

  test('hides the toggle when the backend reports agent mode', async ({ page }) => {
    await reloadWithFeatures(page, true);
    await expect(page.getByTestId('chat-kb-toggle')).toHaveCount(0);
    await expect(page.getByTestId('chat-form')).toBeVisible();
  });

  test('shows the toggle when the backend reports legacy mode', async ({ page }) => {
    await reloadWithFeatures(page, false);
    await expect(page.getByTestId('chat-kb-toggle')).toBeVisible();
  });

  test('matches the live backend feature probe', async ({ page }) => {
    const token = await readAccessToken(page);
    const response = await page.request.get(`${API_URL}/features`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(response.ok()).toBe(true);
    const body = (await response.json()) as { agent_enabled?: unknown };
    if (body.agent_enabled === true) {
      await expect(page.getByTestId('chat-kb-toggle')).toHaveCount(0);
    } else {
      await expect(page.getByTestId('chat-kb-toggle')).toBeVisible();
    }
  });
});
