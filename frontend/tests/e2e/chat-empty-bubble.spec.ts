import { test, expect, type Page, type Route } from '@playwright/test';

/**
 * Browser-level verification that the chat store drops a stranded
 * empty assistant bubble on a mid-stream `error` event, and keeps
 * the partial row when at least one delta had already landed.
 *
 * Unit tests in `src/features/chat/store/chatStore.test.ts` cover
 * the same branches in isolation; this file exercises the full
 * fetch → SSE parser → store → render path so a wire-shape or
 * DOM-ordering regression in the middle of the chain can't pass
 * the unit tests while breaking the real UI.
 *
 * The dev stack must be up: `make up` (db/redis/jaeger) +
 * `make dev` (backend on :8080, frontend on :5173). The
 * playwright.config.ts only manages the frontend dev server; the
 * others are preconditions.
 */

const TEST_USER = {
  username: `e2e_empty_${Date.now()}_${Math.floor(Math.random() * 10000)}`,
  password: 'testpass123',
};

async function registerAndOpenChat(page: Page): Promise<void> {
  await page.goto('/');
  await page.getByRole('button', { name: /Login \/ Register/i }).click();
  await page.getByRole('button', { name: /Register/i }).last().click();
  // AuthForm uses <Label htmlFor="username"> / htmlFor="password">,
  // no placeholder attribute. Use label-association.
  await page.getByLabel(/Username/i).fill(TEST_USER.username);
  await page.getByLabel(/Password/i).fill(TEST_USER.password);
  await page.getByRole('button', { name: /^Register$/i }).click();
  await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
  await page.getByRole('button', { name: 'Open chat' }).click();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

/**
 * Stub POST /api/chat with a hand-crafted SSE body. The chat
 * service uses raw `fetch` (not axios), so the route intercept
 * sees the request and can return whatever stream frames the
 * test wants.
 */
async function stubChatSse(
  page: Page,
  frames: Array<Record<string, unknown>>,
): Promise<void> {
  const body =
    frames
      .map((f) => `event: ${f.kind}\ndata: ${JSON.stringify(f)}\n\n`)
      .join('') + 'data: [DONE]\n\n';
  await page.route('**/api/chat', async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: 'text/event-stream',
      headers: { 'cache-control': 'no-cache' },
      body,
    });
  });
}

test.describe('Chat empty-bubble on mid-stream error (browser-level)', () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page);
  });

  test('error before any delta drops the assistant row, no stranded empty bubble', async ({
    page,
  }) => {
    await stubChatSse(page, [
      {
        kind: 'error',
        error: 'upstream failed',
        request_id: 'req-empty',
      },
    ]);

    const input = page.getByTestId('chat-input');
    await input.fill('hi');
    await page.getByTestId('chat-send').click();

    // User message lands (optimistic).
    await expect(
      page.getByTestId('chat-message-user').getByText('hi'),
    ).toBeVisible();

    // Stream ends — cancel button vanishes.
    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 10000,
    });

    // No stranded empty assistant bubble. The user message is the
    // only row left in the message list.
    await expect(page.getByTestId('chat-message-assistant')).toHaveCount(0);

    // Error toast surfaces the safe static error text.
    const err = page.getByTestId('chat-error');
    await expect(err).toBeVisible();
    await expect(err).toHaveText(/upstream failed/);
  });

  test('error after a delta keeps the partial assistant row', async ({
    page,
  }) => {
    await stubChatSse(page, [
      { kind: 'delta', delta: 'partial answer ' },
      {
        kind: 'error',
        error: 'upstream failed',
        request_id: 'req-partial',
      },
    ]);

    const input = page.getByTestId('chat-input');
    await input.fill('hi');
    await page.getByTestId('chat-send').click();

    await expect(
      page.getByTestId('chat-message-user').getByText('hi'),
    ).toBeVisible();

    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 10000,
    });

    // The partial content stays — the user already saw it.
    const assistant = page.getByTestId('chat-message-assistant').last();
    await expect(assistant).toBeVisible();
    const text = (await assistant.innerText()).trim();
    expect(text).toBe('partial answer');

    // Error toast still surfaces the truncation.
    const err = page.getByTestId('chat-error');
    await expect(err).toBeVisible();
    await expect(err).toHaveText(/upstream failed/);
  });

  test('send after a no-delta error does not stack empty assistant rows', async ({
    page,
  }) => {
    // Reproduces the upstream cause of the 3-stacked-history bug:
    // a previous error-then-send cycle used to leave an empty
    // assistant row in `messages`; the next send sent that empty
    // pair to the planner, which then re-planned for the assistant.
    // After the fix, the message list starts the second turn with
    // just the user-1 row, so the planner sees a clean history.
    await stubChatSse(page, [
      { kind: 'error', error: 'first failure', request_id: 'req-1' },
    ]);
    const input = page.getByTestId('chat-input');
    await input.fill('first');
    await page.getByTestId('chat-send').click();
    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 10000,
    });
    await expect(page.getByTestId('chat-message-assistant')).toHaveCount(0);

    // Second turn — same stub. The next stream also errors before
    // any delta, so the assistant row is still empty.
    await stubChatSse(page, [
      { kind: 'error', error: 'second failure', request_id: 'req-2' },
    ]);
    await input.fill('second');
    await page.getByTestId('chat-send').click();
    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 10000,
    });

    // Two user rows, zero assistant rows. The first error left no
    // stranded bubble; the second error left no stranded bubble.
    // The message list is exactly user-1, user-2.
    const userRows = page.getByTestId('chat-message-user');
    expect(await userRows.count()).toBe(2);
    await expect(page.getByTestId('chat-message-assistant')).toHaveCount(0);
  });
});
