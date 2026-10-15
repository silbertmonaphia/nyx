import { test, expect, type Page } from '@playwright/test';

/**
 * Live feed-creation approval flow — agent-feed-tools task 7.2.
 *
 * The canned sibling (chat-feed-approval.spec.ts) pins the wire bytes and
 * the pause/resume mechanics against fixtures; THIS spec drives the real
 * thing end to end: live planner/executor → Go MCP server → a real
 * `confirm_request` pause → a real decision POST → the feed landing in
 * Postgres (visible on the feeds page after a reload).
 *
 * Environment: PLAYWRIGHT_BASE_URL must point at a stack running THIS
 * worktree's feature code (scripts/e2e-side-stack.sh up — SPA :5274,
 * backend :8181, agent :9102), with MCP_ENABLED=true and a reachable LLM
 * (host vLLM on :8000). The live planner is a small model, so the prompt
 * names the tool explicitly — same flake posture as
 * chat-agent-planner.spec.ts ("loosen only with recorded evidence").
 *
 * Side effects: creates one feed for a throwaway user (unique per run).
 */

// Fresh user per test — see chat-rag.spec.ts `makeTestUser` for why a
// module-level constant re-registered in beforeEach collides (409).
function makeTestUser() {
  return {
    username: `e2e_feed_live_${Date.now()}_${Math.floor(Math.random() * 100000)}`,
    password: 'testpass123',
  };
}

async function registerAndOpenChat(page: Page): Promise<void> {
  const TEST_USER = makeTestUser();
  await page.goto('/');
  await page.getByRole('button', { name: /Login \/ Register/i }).click();
  await page.getByRole('button', { name: /Register/i }).last().click();
  await page.getByLabel(/Username/i).fill(TEST_USER.username);
  await page.getByLabel(/Password/i).fill(TEST_USER.password);
  await page.getByRole('button', { name: /^Register$/i }).click();
  await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
  await page.getByRole('button', { name: 'Open chat' }).click();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

const FEED_NAME = `Live Smoke ${Date.now()}`;
const FEED_URL = 'https://example.com/feed.xml';

test.describe('Feed-tool approval flow (live stack)', { tag: '@live' }, () => {
  test('create_feed pauses on the card, approve executes, feed lands', async ({
    page,
  }) => {
    await registerAndOpenChat(page);
    await page
      .getByTestId('chat-input')
      .fill(
        `Use the create_feed tool to add a feed with title "${FEED_NAME}" and url ${FEED_URL}.`,
      );
    await page.getByTestId('chat-send').click();

    // The mutating call pauses the run on the confirmation card (the live
    // planner may take several steps first — generous window).
    const card = page.getByTestId('chat-confirm-card');
    await expect(card).toBeVisible({ timeout: 120_000 });
    await expect(page.getByTestId('chat-confirm-tool')).toHaveText('create_feed');
    await expect(page.getByTestId('chat-confirm-args')).toContainText(FEED_NAME);

    await page.getByTestId('chat-confirm-approve').click();
    await expect(card).toHaveAttribute('data-decision', 'approved', {
      timeout: 30_000,
    });

    // The run resumes and settles cleanly — the approved call executes.
    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 120_000,
    });
    await expect(page.getByTestId('chat-error')).toHaveCount(0);

    // Close the chat and reload: the feeds page shows the created row —
    // proof the decision executed against the real registry + Postgres.
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('chat-form')).not.toBeVisible();
    await page.reload();
    await expect(page.getByTestId('feed-list')).toContainText(FEED_NAME, {
      timeout: 15_000,
    });
  });
});
