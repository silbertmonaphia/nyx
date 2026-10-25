import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { registerAndOpenChat as registerAndOpenChatShared } from './helpers/llm-config';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Tool-dispatch retry — browser verification for `agent-tool-dispatch-retry`.
 *
 * Three contracts beyond `chat-agent-planner.spec.ts`:
 *   - a tool call that fails validation, is re-prompted, and succeeds
 *     renders the step as `ok` WITH a retry badge (↻ N) and NO
 *     partial-failure banner;
 *   - a tool call whose retry budget is exhausted renders the step as
 *     `failed`, the answer still streams, and a partial-failure banner
 *     with `data-failed-steps` lists the failed step id — visible WITHOUT
 *     expanding the agent-steps disclosure;
 *   - a clean run renders no banner.
 *
 * The deterministic retry paths are replayed from recorded-shape wire
 * bytes (fixtures/tool-retry-*.sse) through the frontend's own SSE parser
 * via route fulfillment — no live model involved. The frame shapes are
 * pinned byte-for-byte by the Python producer tests
 * (backend/agent_py/tests/test_executor.py :: test_tool_result_wire_fixture_retry_count
 *  and :: test_partial_failure_note_wire_fixture); if the producer drifts,
 * those tests and this spec fail together. When the rollout gate
 * (openspec/changes/agent-tool-dispatch-retry-rollout/ §2.1) runs against
 * the live stack, replace these fixtures with real captures if drift is
 * suspected.
 *
 * The first describe drives a live chat (needs `make up` + `make dev` +
 * a reachable LLM provider — same preconditions as chat-agent-planner
 * .spec.ts). Deterministically forcing a live retry requires the
 * controllable provider stub from the rollout sibling's probe/ directory
 * (rollout §2.2); until that is wired into the dev stack, the fixture
 * describes below are the deterministic substitute for the retry paths.
 */

const FIXTURES = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  'fixtures',
);

function fixture(name: string): string {
  return readFileSync(path.join(FIXTURES, name), 'utf8');
}

async function registerAndOpenChat(page: Page): Promise<void> {
  await registerAndOpenChatShared(page, { prefix: 'e2e_retry' });
}

test.describe('Chat / tool retry (live stack)', { tag: '@live' }, () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page);
  });

  test('a live tool-step run renders terminal steps and a final answer', async ({
    page,
  }) => {
    const input = page.getByTestId('chat-input');
    await input.fill(
      'Use the list_feeds tool to list my feeds, then summarize what you found.',
    );
    await page.getByTestId('chat-send').click();

    // Stream terminates cleanly (the cancel button is only mounted
    // mid-stream; its absence after settling is the finish signal).
    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 30000,
    });

    // The agent-steps disclosure rendered and every row reached a
    // terminal status — nothing left spinning.
    const stepsDisclosure = page.getByTestId('chat-agent-steps').last();
    await expect(stepsDisclosure).toBeVisible();
    const stepRows = stepsDisclosure.getByTestId('chat-agent-step');
    for (const row of await stepRows.all()) {
      const status = await row.getAttribute('data-step-status');
      expect(['ok', 'failed']).toContain(status);
    }

    // The final assistant reply is non-empty.
    const assistant = page.getByTestId('chat-message-assistant').last();
    await expect(assistant).not.toHaveText(/^\s*$/, { timeout: 10000 });
  });
});

test.describe('Chat / tool retry frames (recorded wire bytes)', () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page);
  });

  test('retry-recovered stream renders the retry badge and no banner', async ({
    page,
  }) => {
    await page.route('**/api/chat*', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: fixture('tool-retry-recovered.sse'),
      }),
    );

    await page.getByTestId('chat-input').fill('hi');
    await page.getByTestId('chat-send').click();

    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 15000,
    });
    await expect(page.getByTestId('chat-error')).toHaveCount(0);

    // The step completed, WITH the retry badge (retry_count=1 on the
    // terminal tool_result frame).
    const disclosure = page.getByTestId('chat-agent-steps').last();
    await expect(disclosure).toBeVisible();
    const isOpen = await disclosure.evaluate(
      (el) => (el as HTMLDetailsElement).open,
    );
    if (!isOpen) {
      await disclosure.locator(':scope > summary').click();
    }
    const row = disclosure.getByTestId('chat-agent-step').first();
    await expect(row).toHaveAttribute('data-step-status', 'ok');
    await expect(
      disclosure.getByTestId('chat-agent-step-retry').first(),
    ).toHaveText(/1/);

    // The eventual answer rendered …
    const assistant = page.getByTestId('chat-message-assistant').last();
    await expect(assistant).toContainText('Example', { timeout: 5000 });

    // … and no partial-failure banner: the retry recovered within budget.
    await expect(page.getByTestId('chat-partial-failure-banner')).toHaveCount(
      0,
    );
  });

  test('retry-exhausted stream renders the partial-failure banner above the answer', async ({
    page,
  }) => {
    await page.route('**/api/chat*', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: fixture('tool-retry-exhausted.sse'),
      }),
    );

    await page.getByTestId('chat-input').fill('hi');
    await page.getByTestId('chat-send').click();

    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 15000,
    });

    // The banner is visible WITHOUT expanding the disclosure, and names
    // the failed step via `data-failed-steps`.
    const banner = page.getByTestId('chat-partial-failure-banner');
    await expect(banner).toBeVisible();
    await expect(banner).toHaveAttribute('data-failed-steps', 'step_1');
    await expect(banner).toHaveText(/may be incomplete/);

    // The answer still streamed — the run was best-effort, not aborted.
    const assistant = page.getByTestId('chat-message-assistant').last();
    await expect(assistant).toContainText('could not fetch', {
      timeout: 5000,
    });

    // The failed step is marked failed in the trace.
    const disclosure = page.getByTestId('chat-agent-steps').last();
    const isOpen = await disclosure.evaluate(
      (el) => (el as HTMLDetailsElement).open,
    );
    if (!isOpen) {
      await disclosure.locator(':scope > summary').click();
    }
    await expect(
      disclosure.getByTestId('chat-agent-step').first(),
    ).toHaveAttribute('data-step-status', 'failed');
  });
});
