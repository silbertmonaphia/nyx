import { expect, type Page, type Route } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { registerAndOpenChat } from './helpers/llm-config';

async function stubResumeRoutes(
  page: Page,
  opts: {
    reattach: string | null;
    resume: string | null;
    transcript?: Array<{ id: number; role: string; content: string }>;
  },
): Promise<{ reattachCalls: () => number; resumeCalls: () => number }> {
  let reattachCalls = 0;
  let resumeCalls = 0;
  let primaryPosts = 0;

  await page.route('**/api/chat/runs/*/stream', async (route: Route) => {
    reattachCalls += 1;
    if (opts.reattach === null) {
      await route.fulfill({
        status: 409,
        contentType: 'application/json',
        body: JSON.stringify({ error: 'Run is not live', code: 'run_not_live' }),
      });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: 'text/event-stream',
      headers: { 'cache-control': 'no-cache' },
      body: opts.reattach,
    });
  });

  await page.route('**/api/chat/sessions/*/messages', async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(opts.transcript ?? []),
    });
  });

  await page.route('**/api/chat', async (route: Route) => {
    if (route.request().method() !== 'POST') {
      await route.fallback();
      return;
    }
    primaryPosts += 1;
    if (primaryPosts === 1) {
      await route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        headers: { 'cache-control': 'no-cache' },
        body: CUT_STREAM,
      });
      return;
    }
    resumeCalls += 1;
    if (opts.resume === null) {
      await route.fulfill({
        status: 500,
        contentType: 'application/json',
        body: JSON.stringify({ error: 'boom' }),
      });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: 'text/event-stream',
      headers: { 'cache-control': 'no-cache' },
      body: opts.resume,
    });
  });

  return {
    reattachCalls: () => reattachCalls,
    resumeCalls: () => resumeCalls,
  };
}

const CUT_STREAM =
  'event: run_id\ndata: {"run_id":"11111111-1111-4111-8111-111111111111"}\n\n' +
  'event: delta\ndata: {"delta":"first half "}\n\n' +
  'event: delta\ndata: {"delta":"second half"}\n\n';

const REATTACH_STREAM =
  'event: note\ndata: {"text":"reattached","kind":"reattached"}\n\n' +
  'event: delta\ndata: {"delta":"rest of the answer"}\n\n' +
  'event: done\ndata: {"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}\n\n' +
  'data: [DONE]\n\n';

test.describe('Chat stream resilience (browser-level)', () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page, { prefix: 'e2e_resume' });
  });

  test('a cut stream re-attaches and completes the turn', async ({ page }) => {
    const counters = await stubResumeRoutes(page, {
      reattach: REATTACH_STREAM,
      resume: REATTACH_STREAM,
    });

    await page.getByTestId('chat-input').fill('hi');
    await page.getByTestId('chat-send').click();

    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).toContainText('first half', { timeout: 15_000 });

    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).toContainText('rest of the answer', { timeout: 20_000 });

    await expect(page.getByTestId('chat-message-assistant')).toHaveCount(1);
    expect(counters.reattachCalls()).toBeGreaterThan(0);
    await expect(page.getByTestId('chat-error')).toHaveCount(0);
  });

  test('a cut stream with no live run replays the stored transcript', async ({
    page,
  }) => {
    const counters = await stubResumeRoutes(page, {
      reattach: null,
      resume: null,
      transcript: [
        { id: 1, role: 'user', content: 'hi' },
        { id: 2, role: 'assistant', content: 'the stored answer' },
      ],
    });

    await page.getByTestId('chat-input').fill('hi');
    await page.getByTestId('chat-send').click();

    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).toContainText('the stored answer', { timeout: 20_000 });
    expect(counters.reattachCalls()).toBeGreaterThan(0);
    await expect(page.getByTestId('chat-error')).toHaveCount(0);
  });

  test('an unrecoverable stream surfaces the interruption and keeps the partial answer', async ({
    page,
  }) => {
    await stubResumeRoutes(page, {
      reattach: null,
      resume: null,
      transcript: [],
    });

    await page.getByTestId('chat-input').fill('hi');
    await page.getByTestId('chat-send').click();

    await expect(page.getByTestId('chat-error')).toBeVisible({
      timeout: 30_000,
    });
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).toContainText('first half');
  });

  test('a live re-attach never opens a second assistant bubble', async ({
    page,
  }) => {
    await stubResumeRoutes(page, {
      reattach: REATTACH_STREAM,
      resume: REATTACH_STREAM,
    });

    await page.getByTestId('chat-input').fill('hi');
    await page.getByTestId('chat-send').click();
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).toContainText('rest of the answer', { timeout: 20_000 });

    await page.waitForTimeout(2000);
    await expect(page.getByTestId('chat-message-assistant')).toHaveCount(1);
    await expect(page.getByTestId('chat-message-user')).toHaveCount(1);
  });
});
