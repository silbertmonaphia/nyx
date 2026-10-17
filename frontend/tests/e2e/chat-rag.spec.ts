import { expect, type Page, type Locator } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';

/**
 * Browser-level verification of the chat / RAG flow.
 *
 * What unit tests can't catch (this file exists to fill the gap):
 *   - SSE chunk-boundary parsing — the parser walks a line buffer
 *     that may split a frame across reads.
 *   - Auto-scroll during streaming — uses requestAnimationFrame so
 *     a burst of deltas queues scroll writes correctly.
 *   - AbortController race: cancelling mid-stream must drop the
 *     half-built assistant row, not leave an empty bubble.
 *   - Dialog close → reset semantics: closing the chat panel must
 *     wipe the ephemeral message history.
 *   - Multi-turn order: assistant reply of turn N must sit below
 *     user message of turn N+1, not get scrambled by stream state.
 *
 * Assumes the dev stack is up: `make up` (db/redis/jaeger) +
 * `make dev` (backend on :8080, frontend on :5173, Python agent
 * on :9000). The playwright.config.ts only manages the frontend
 * dev server; the others are preconditions.
 */

// Fresh user per test. `registerAndOpenChat` runs in `beforeEach` and
// registers through the UI every time, so the name must be unique within
// this file too: a module-level constant collides with itself in any
// worker that executes a second test from the same file → 409 "User
// already exists" → the success toast never appears and the test fails
// in its beforeEach (observed 2026-10-13, the root of the suite's
// registration flake).
function makeTestUser() {
  return {
    username: `e2e_chat_${Date.now()}_${Math.floor(Math.random() * 100000)}`,
    password: 'testpass123',
  };
}

async function registerAndOpenChat(page: Page): Promise<void> {
  const TEST_USER = makeTestUser();
  await page.goto('/');
  await page.getByRole('button', { name: /Register/i }).last().click();
  await page.getByLabel(/Username/i).fill(TEST_USER.username);
  await page.getByLabel(/Password/i).fill(TEST_USER.password);
  await page.getByRole('button', { name: /^Register$/i }).click();
  await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

async function closeChat(page: Page): Promise<void> {
  await page.keyboard.press('Escape');
  await expect(page.getByTestId('chat-form')).not.toBeVisible();
}

async function sendMessage(page: Page, text: string): Promise<void> {
  const input = page.getByTestId('chat-input');
  await input.fill(text);
  await page.getByTestId('chat-send').click();
}

async function waitForStreamEnd(
  page: Page,
  opts: { timeout?: number; expectAtLeastOneDelta?: boolean } = {},
): Promise<void> {
  // The Cancel button is only mounted while streaming; its
  // disappearance means the stream terminated cleanly. This is
  // the same signal ChatPanel uses internally — the send button
  // is disabled during streaming, so the test reads the same
  // "isStreaming" bit by waiting for the cancel button to vanish.
  await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
    timeout: opts.timeout ?? 15000,
  });
  if (opts.expectAtLeastOneDelta !== false) {
    // Sanity: at least one assistant bubble has visible text.
    const lastAssistant = page.getByTestId('chat-message-assistant').last();
    await expect(lastAssistant).toBeVisible({ timeout: 5000 });
    await expect(lastAssistant).not.toHaveText(/^\s*$/, { timeout: 5000 });
  }
}

test.describe('Chat / RAG flow (browser-level)', { tag: '@live' }, () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page);
  });

  test('streams a single-turn reply with the agent step trace', async ({
    page,
  }) => {
    await sendMessage(page, 'Say exactly: hello-from-browser');

    // The user bubble must land immediately (optimistic).
    await expect(
      page.getByTestId('chat-message-user').getByText('Say exactly: hello-from-browser'),
    ).toBeVisible();

    // Assistant bubble seeded as soon as streaming starts; its
    // content is empty until the first delta. Wait for the bubble
    // itself before asserting on its text — otherwise `last()`
    // could match a stale node.
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).toBeVisible();

    // Wait for the stream to finish — cancel button vanishes.
    await waitForStreamEnd(page);

    // Final assistant content should be non-empty.
    const assistant = page.getByTestId('chat-message-assistant').last();
    const finalText = (await assistant.innerText()).trim();
    expect(finalText.length).toBeGreaterThan(0);

    // No error frame should have surfaced on the happy path.
    await expect(page.getByTestId('chat-error')).toHaveCount(0);

    // Step trace disclosure is rendered for agent-loop runs.
    // The agent emits one `plan` with an `llm` step, so we expect
    // at least one row carrying data-step-status.
    await expect(page.getByTestId('chat-agent-steps')).toBeVisible();
    const stepRows = page.getByTestId('chat-agent-step');
    expect(await stepRows.count()).toBeGreaterThan(0);
    // Every step row should have reached a terminal status by now.
    const rowHandles = await stepRows.all();
    for (const row of rowHandles) {
      const status = await row.getAttribute('data-step-status');
      expect(['ok', 'failed']).toContain(status);
    }
  });

  test('multi-turn preserves message order and prior assistant reply', async ({
    page,
  }) => {
    await sendMessage(page, 'first turn question');
    await waitForStreamEnd(page);

    await sendMessage(page, 'second turn question');
    await waitForStreamEnd(page);

    const userBubbles = page.getByTestId('chat-message-user');
    const assistantBubbles = page.getByTestId('chat-message-assistant');
    expect(await userBubbles.count()).toBe(2);
    expect(await assistantBubbles.count()).toBe(2);

    // Order: user-1, assistant-1, user-2, assistant-2.
    const allRows = page
      .getByTestId('chat-message-list')
      .locator('[data-testid^="chat-message-"]');
    const ids: string[] = [];
    for (const row of await allRows.all()) {
      const tid = await row.getAttribute('data-testid');
      ids.push(tid ?? '');
    }
    expect(ids).toEqual([
      'chat-message-user',
      'chat-message-assistant',
      'chat-message-user',
      'chat-message-assistant',
    ]);
  });

  test('Cancel mid-stream drops the half-built assistant row', async ({
    page,
  }) => {
    await sendMessage(page, 'tell me a long story about a penguin');

    // Cancel button must appear while streaming.
    await expect(page.getByTestId('chat-cancel')).toBeVisible({
      timeout: 5000,
    });

    // Fire cancel before the stream finishes. The store's race-
    // guarded abort path should drop the assistant row.
    await page.getByTestId('chat-cancel').click();

    // After cancel settles: cancel button gone, no stranded
    // assistant bubble, no error frame surfaced (Cancel is a
    // user-driven abort, not an error).
    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, { timeout: 10000 });
    // The user message must remain; the assistant message must NOT.
    await expect(
      page.getByTestId('chat-message-user').getByText('tell me a long story about a penguin'),
    ).toBeVisible();
    await expect(page.getByTestId('chat-message-assistant')).toHaveCount(0);
    await expect(page.getByTestId('chat-error')).toHaveCount(0);
  });

  test('closing and reopening the dialog restores the active conversation', async ({
    page,
  }) => {
    // agent-chat-history-ui changed this contract: closing the dialog no
    // longer wipes the conversation. Reopening restores the active
    // session and replays its stored turns — the product goal is that
    // closing the window must NOT lose the chat (docs_agent/AgentMemoryDesign.md
    // pain point 2). A fresh conversation starts only via the explicit
    // `+ New chat` action (covered in chat-history-sidebar.spec.ts).
    await sendMessage(page, 'remember me');
    await waitForStreamEnd(page);
    expect(
      await page.getByTestId('chat-message-user').count(),
    ).toBeGreaterThan(0);

    await closeChat(page);
    await page.getByRole('button', { name: 'Open AI Agent' }).click();
    await expect(page.getByTestId('chat-form')).toBeVisible();

    // The replayed turn comes back over the session REST endpoints
    // (async), so allow the fetch to land.
    await expect(page.getByTestId('chat-message-user').last()).toHaveText(
      'remember me',
      { timeout: 15000 },
    );
    // The assistant half must replay too — a user-only replay means the
    // turn's terminal persistence was lost (the disconnect race the
    // finalize shield in agent/main.py closes).
    await expect(page.getByTestId('chat-message-assistant').last()).not.toHaveText(
      /^\s*$/,
      { timeout: 15000 },
    );
  });

  test('Enter sends, Shift+Enter inserts a newline', async ({ page }) => {
    const input = page.getByTestId('chat-input');
    await input.click();
    await input.type('line one');
    await input.press('Enter');
    // After Enter submit: input must be cleared AND a user bubble
    // must have landed.
    await expect(input).toHaveValue('');
    await expect(
      page.getByTestId('chat-message-user').getByText('line one'),
    ).toBeVisible();
    await waitForStreamEnd(page);

    // Shift+Enter must NOT submit — the newline must stay in the
    // textarea so the user can compose multi-line input.
    await input.click();
    await input.type('line a');
    await input.press('Shift+Enter');
    await input.type('line b');
    await expect(input).toHaveValue('line a\nline b');
    // No new user bubble landed.
    expect(
      await page.getByTestId('chat-message-user').count(),
    ).toBe(1);
  });

  test('chat button is hidden when unauthenticated', async ({ page }) => {
    // Logout via the nav button to flip auth store.
    await closeChat(page);
    await page.getByRole('button', { name: /^Logout$/i }).click();
    // Now the Chat button should not be present.
    await expect(
      page.getByRole('button', { name: 'Open AI Agent' }),
    ).toHaveCount(0);
  });
});
