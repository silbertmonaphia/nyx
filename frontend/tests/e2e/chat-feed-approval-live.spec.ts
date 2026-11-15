import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { registerAndOpenChat as registerAndOpenChatShared } from './helpers/llm-config';

/**
 * Live feed-creation approval flow — agent-feed-tools task 7.2.
 *
 * The canned sibling (chat-feed-approval.spec.ts) pins the wire bytes and
 * the pause/resume mechanics against fixtures; THIS spec drives the real
 * thing end to end: live planner/executor → Go MCP server → a real
 * `confirm_request` pause → a real decision POST → the feed landing in
 * Postgres (visible on the feeds page as soon as the chat panel closes).
 *
 * Environment: PLAYWRIGHT_BASE_URL must point at a stack running THIS
 * worktree's feature code (scripts/e2e-side-stack.sh up — SPA :5274,
 * backend :8181, agent :9102), MCP_ENABLED=true, short PYTHON_AGENT_APPROVAL_TTL_SECONDS (E2E_APPROVAL_TTL_SECONDS required), reachable LLM
 * (host vLLM on :8000). The live planner is a small model, so the prompt
 * names the tool explicitly — same flake posture as
 * chat-agent-planner.spec.ts ("loosen only with recorded evidence").
 *
 * Side effects: a throwaway user per test; the approve test creates one feed, the expiry test creates none.
 */

async function registerAndOpenChat(page: Page): Promise<void> {
  await registerAndOpenChatShared(page, { prefix: 'e2e_feed_live' });
}

const FEED_NAME = `Live Smoke ${Date.now()}`;
const EXPIRING_FEED_NAME = `Live Expiry ${Date.now()}`;
const FEED_URL = 'https://example.com/feed.xml';

function readApprovalTtlSeconds(): number {
  const raw = process.env.E2E_APPROVAL_TTL_SECONDS;
  const parsed =
    raw === undefined || raw.trim() === '' ? Number.NaN : Number(raw);
  if (!Number.isFinite(parsed) || parsed <= 0) {
    throw new Error(
      `E2E_APPROVAL_TTL_SECONDS must match the live stack's short PYTHON_AGENT_APPROVAL_TTL_SECONDS (got ${raw === undefined ? 'unset' : JSON.stringify(raw)})`,
    );
  }
  return parsed;
}

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

    // Close the chat: the feeds page shows the created row without a
    // reload — proof the decision executed and the cache was refreshed.
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('chat-form')).not.toBeVisible();
    await expect(page.getByTestId('feed-list')).toContainText(FEED_NAME, {
      timeout: 15_000,
    });
  });

  test('create_feed confirmation expires unanswered: the answer reports no execution', async ({
    page,
  }) => {
    const approvalTtlSeconds = readApprovalTtlSeconds();
    test.setTimeout(approvalTtlSeconds * 2000 + 360_000);

    await registerAndOpenChatShared(page, { prefix: 'e2e_feed_expire' });
    await page
      .getByTestId('chat-input')
      .fill(
        `Use the create_feed tool to add a feed with title "${EXPIRING_FEED_NAME}" and url ${FEED_URL}.`,
      );
    await page.getByTestId('chat-send').click();

    const card = page.getByTestId('chat-confirm-card');
    await expect(card).toBeVisible({ timeout: 120_000 });
    await expect(page.getByTestId('chat-confirm-tool')).toHaveText('create_feed');
    await expect(page.getByTestId('chat-confirm-args')).toContainText(
      EXPIRING_FEED_NAME,
    );

    await expect(card).toHaveAttribute('data-decision', 'expired', {
      timeout: approvalTtlSeconds * 1000 + 60_000,
    });

    const answerTimeout = approvalTtlSeconds * 1000 + 120_000;
    const answer = page
      .getByTestId('chat-message-assistant')
      .last()
      .getByTestId('chat-message-content');
    await expect(answer).toContainText('create_feed', {
      timeout: answerTimeout,
    });
    await expect(answer).toContainText(/expired|not executed/i, {
      timeout: answerTimeout,
    });

    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: answerTimeout,
    });
    await expect(page.getByTestId('chat-error')).toHaveCount(0);
    await expect(answer).not.toContainText(
      /successfully created|created (?:your|the) feed|the feed (?:was|has been) created/i,
    );
  });
});
