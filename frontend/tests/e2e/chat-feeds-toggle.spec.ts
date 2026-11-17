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

test.describe('Feeds toggle capability', () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page, { prefix: 'e2e_feeds_toggle' });
  });

  test('hides the toggle when the backend reports agent mode', async ({ page }) => {
    await reloadWithFeatures(page, true);
    await expect(page.getByTestId('chat-feeds-toggle')).toHaveCount(0);
    await expect(page.getByTestId('chat-form')).toBeVisible();
  });

  test('shows the toggle when the backend reports legacy mode', async ({ page }) => {
    await reloadWithFeatures(page, false);
    await expect(page.getByTestId('chat-feeds-toggle')).toBeVisible();
  });

  test('sends rag:true only after the toggle is checked', async ({ page }) => {
    await reloadWithFeatures(page, false);
    const bodies: Array<Record<string, unknown>> = [];
    await page.route('**/api/chat', async (route) => {
      if (route.request().method() === 'POST') {
        bodies.push(route.request().postDataJSON() as Record<string, unknown>);
      }
      await route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: 'data: [DONE]\n\n',
      });
    });

    await expect(page.getByTestId('chat-feeds-toggle')).not.toBeChecked();
    await page.getByTestId('chat-input').fill('hello');
    await page.getByTestId('chat-send').click();
    await expect.poll(() => bodies.length).toBe(1);
    expect(bodies[0]).not.toHaveProperty('rag');

    await page.getByTestId('chat-feeds-toggle').check();
    await page.getByTestId('chat-input').fill('hello again');
    await page.getByTestId('chat-send').click();
    await expect.poll(() => bodies.length).toBe(2);
    expect(bodies[1].rag).toBe(true);
  });

  test('matches the live backend feature probe', async ({ page }) => {
    const token = await readAccessToken(page);
    const response = await page.request.get(`${API_URL}/features`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(response.ok()).toBe(true);
    const body = (await response.json()) as { agent_enabled?: unknown };
    if (body.agent_enabled === true) {
      await expect(page.getByTestId('chat-feeds-toggle')).toHaveCount(0);
    } else {
      await expect(page.getByTestId('chat-feeds-toggle')).toBeVisible();
    }
  });
});
