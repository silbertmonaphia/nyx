import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import {
  API_URL,
  readAccessToken,
  registerAndOpenChat as registerAndOpenChatShared,
} from './helpers/llm-config';

test.describe('Tool-call idempotency (live stack)', { tag: '@live' }, () => {
  test('a lost create_feed response retries with the same key and creates one feed', async ({
    page,
  }) => {
    test.skip(
      process.env.E2E_TOOL_IDEMPOTENCY_FAULT !== 'true',
      'requires a side stack started with PYTHON_AGENT_TEST_LOSE_TOOL_RESULT_ONCE=true',
    );
    const feedTitle = `Idempotent ${Date.now()}-${Math.floor(Math.random() * 100000)}`;

    await registerAndOpenChatShared(page, { prefix: 'e2e_idem' });
    await page
      .getByTestId('chat-input')
      .fill(
        `Use the create_feed tool to add a feed with title "${feedTitle}" and description "idempotency e2e".`,
      );
    await page.getByTestId('chat-send').click();

    const card = page.getByTestId('chat-confirm-card');
    await expect(card).toBeVisible({ timeout: 120_000 });
    await expect(page.getByTestId('chat-confirm-tool')).toHaveText('create_feed');
    await expect(page.getByTestId('chat-confirm-args')).toContainText(feedTitle);

    await page.getByTestId('chat-confirm-approve').click();
    await expect(card).toHaveAttribute('data-decision', 'approved', {
      timeout: 30_000,
    });

    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 120_000,
    });
    await expect(page.getByTestId('chat-error')).toHaveCount(0);

    const createStep = page
      .getByTestId('chat-agent-step')
      .filter({ hasText: 'create_feed' });
    await expect(createStep.getByTestId('chat-agent-step-retry')).toHaveText(
      /↻\s*1/,
    );

    await page.keyboard.press('Escape');
    await expect(page.getByTestId('chat-form')).not.toBeVisible();
    await expect(page.getByTestId('feed-list')).toContainText(feedTitle, {
      timeout: 15_000,
    });

    const token = await readAccessToken(page);
    const response = await page.request.get(
      `${API_URL}/feeds?query=${encodeURIComponent(feedTitle)}`,
      { headers: { Authorization: `Bearer ${token}` } },
    );
    expect(response.ok(), `GET ${API_URL}/feeds failed`).toBeTruthy();
    const body = (await response.json()) as { data: Array<{ title: string }> };
    const matches = body.data.filter((row) => row.title === feedTitle);
    expect(matches, 'the retried create must land exactly one feed row').toHaveLength(1);
  });

  test('two identical turns in one session create two feeds', async ({
    page,
  }) => {
    const feedTitle = `Repeat ${Date.now()}-${Math.floor(Math.random() * 100000)}`;
    const prompt = `Use the create_feed tool to add a feed with title "${feedTitle}" and description "turn id e2e".`;

    await registerAndOpenChatShared(page, { prefix: 'e2e_idem_turns' });

    for (let turn = 0; turn < 2; turn += 1) {
      await page.getByTestId('chat-input').fill(prompt);
      await page.getByTestId('chat-send').click();

      const card = page.getByTestId('chat-confirm-card').last();
      await expect(card).toBeVisible({ timeout: 120_000 });
      await expect(card.getByTestId('chat-confirm-tool')).toHaveText(
        'create_feed',
      );
      await expect(card.getByTestId('chat-confirm-args')).toContainText(
        feedTitle,
      );
      await card.getByTestId('chat-confirm-approve').click();
      await expect(card).toHaveAttribute('data-decision', 'approved', {
        timeout: 30_000,
      });
      await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
        timeout: 120_000,
      });
      await expect(page.getByTestId('chat-error')).toHaveCount(0);
    }

    const token = await readAccessToken(page);
    const response = await page.request.get(
      `${API_URL}/feeds?query=${encodeURIComponent(feedTitle)}`,
      { headers: { Authorization: `Bearer ${token}` } },
    );
    expect(response.ok(), `GET ${API_URL}/feeds failed`).toBeTruthy();
    const body = (await response.json()) as { data: Array<{ title: string }> };
    const matches = body.data.filter((row) => row.title === feedTitle);
    expect(
      matches,
      'a repeated turn in the same session must derive a new key and write again',
    ).toHaveLength(2);
  });
});
