import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { registerAndOpenChat as registerAndOpenChatShared } from './helpers/llm-config';

const API_BASE = process.env.E2E_API_URL ?? 'http://localhost:8080/api';

async function registerAndOpenChat(page: Page): Promise<void> {
  await registerAndOpenChatShared(page, { prefix: 'e2e_feed_refresh' });
}

function frame(kind: string, payload: unknown): string {
  return `event: ${kind}\ndata: ${JSON.stringify(payload)}\n\n`;
}

function createFeedStream(feedTitle: string): string {
  const runId = 'e2e-feed-refresh-run';
  return (
    frame('run_id', { run_id: runId }) +
    frame('note', { text: 'fresh_run', kind: 'fresh_run', run_id: runId }) +
    frame('plan', {
      steps: [
        {
          id: 'step_1',
          kind: 'tool',
          tool: 'create_feed',
          args: { title: feedTitle },
        },
        { id: 'step_2', kind: 'llm', prompt: 'Answer with what happened.' },
      ],
    }) +
    frame('step_start', { step_id: 'step_1', kind: 'tool' }) +
    frame('tool_call', {
      step_id: 'step_1',
      call_id: 'call_1',
      name: 'create_feed',
      args: { title: feedTitle },
    }) +
    frame('tool_result', {
      step_id: 'step_1',
      call_id: 'call_1',
      ok: true,
      result: { id: 1, title: feedTitle },
      error: null,
      retry_count: 0,
    }) +
    frame('step_end', { step_id: 'step_1', status: 'ok', reason: null }) +
    frame('step_start', { step_id: 'step_2', kind: 'llm' }) +
    frame('delta', { delta: 'Created your feed.' }) +
    frame('step_end', { step_id: 'step_2', status: 'ok', reason: null }) +
    frame('done', {
      usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
    }) +
    frame('terminator', { reason: null }) +
    'data: [DONE]\n\n'
  );
}

async function accessToken(page: Page): Promise<string> {
  const token = await page.evaluate(() => {
    for (const storage of [window.localStorage, window.sessionStorage]) {
      const raw = storage.getItem('nyx-token-store');
      if (raw === null) continue;
      const parsed = JSON.parse(raw) as { accessToken?: string };
      if (typeof parsed.accessToken === 'string') return parsed.accessToken;
    }
    return null;
  });
  if (token === null) throw new Error('no auth token in browser storage');
  return token;
}

test.describe('Chat → feed list refresh (browser-level)', () => {
  test('closing the chat panel shows a feed created during the conversation without a reload', async ({
    page,
  }) => {
    await registerAndOpenChat(page);

    await expect(page.getByTestId('feed-list-empty')).toBeVisible();

    const feedTitle = `Agent feed ${Date.now()}`;
    await page.route('**/api/chat', (route) =>
      route.fulfill({
        status: 200,
        headers: {
          'content-type': 'text/event-stream',
          'cache-control': 'no-cache',
        },
        body: createFeedStream(feedTitle),
      }),
    );

    await page.getByTestId('chat-input').fill(`Create a feed called ${feedTitle}`);
    await page.getByTestId('chat-send').click();
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).toContainText('Created your feed.', { timeout: 10_000 });
    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 10_000,
    });

    const token = await accessToken(page);
    const created = await page.request.post(`${API_BASE}/feeds`, {
      headers: { Authorization: `Bearer ${token}` },
      data: { title: feedTitle, description: '' },
    });
    expect(created.status()).toBe(201);

    const refetched = page.waitForResponse(
      (response) =>
        response.url().includes('/api/feeds?') &&
        response.request().method() === 'GET' &&
        response.ok(),
    );
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('chat-form')).not.toBeVisible();

    await refetched;
    await expect(page.getByTestId('feed-list')).toContainText(feedTitle, {
      timeout: 10_000,
    });
  });
});
