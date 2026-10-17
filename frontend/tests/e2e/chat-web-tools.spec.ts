import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Agent-native web tools — browser verification for `agent-web-tools`.
 *
 * A full web-tools run (web_search → web_fetch → answering llm step) is
 * replayed from recorded-shape wire bytes (fixtures/web-tools-search-fetch
 * .sse) through the frontend's own SSE parser via route fulfillment — no
 * live model, no network egress from the agent. The frame shapes are
 * pinned byte-for-byte by the Python producer test
 * (backend/agent_py/tests/test_executor.py :: test_web_tools_wire_fixture_matches_producer,
 * which REGENERATES the stream from the real executor); if the producer
 * drifts, that test and this spec fail together.
 *
 * Contracts under test, beyond chat-agent-retry.spec.ts:
 *   - both tool steps render with their tool names (web_search / web_fetch)
 *     and `ok` status in the agent-steps disclosure;
 *   - the tool_result payloads render as escaped plain text — the
 *     `<untrusted_web_content>` markers must appear as literal text in the
 *     DOM's text content, never as injected HTML elements;
 *   - the final answer streams after the data steps.
 *
 * Prerequisites: a running dev stack (`make up` + `make dev`) — the register
 * flow hits the real backend. The frontend dev server is auto-started by
 * Playwright. The agent/LLM path is bypassed by the route fulfillment.
 *
 * Manual live smoke (NOT a CI gate — needs PYTHON_AGENT_WEB_SEARCH_ENABLED
 * =true + a real PYTHON_AGENT_BRAVE_API_KEY + network egress): with the
 * flags on, open the chat and send "search the web for nyx agent docs and
 * summarize what you find". Expect web_search + web_fetch steps in the
 * trace and a grounded final answer.
 */

const FIXTURES = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  'fixtures',
);

function fixture(name: string): string {
  return readFileSync(path.join(FIXTURES, name), 'utf8');
}

// Fresh user per test — see chat-rag.spec.ts `makeTestUser` for why a
// module-level constant re-registered in beforeEach collides (409).
function makeTestUser() {
  return {
    username: `e2e_webtools_${Date.now()}_${Math.floor(Math.random() * 100000)}`,
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

test.describe('Chat / web tools frames (recorded wire bytes)', () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page);
  });

  test('a web search + fetch run renders both tool steps and the answer', async ({
    page,
  }) => {
    await page.route('**/api/chat*', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: fixture('web-tools-search-fetch.sse'),
      }),
    );

    await page.getByTestId('chat-input').fill(
      'search the web for nyx agent docs and summarize what you find',
    );
    await page.getByTestId('chat-send').click();

    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 15000,
    });
    await expect(page.getByTestId('chat-error')).toHaveCount(0);

    // Open the agent-steps disclosure and check both data steps rendered.
    const disclosure = page.getByTestId('chat-agent-steps').last();
    await expect(disclosure).toBeVisible();
    const isOpen = await disclosure.evaluate(
      (el) => (el as HTMLDetailsElement).open,
    );
    if (!isOpen) {
      await disclosure.locator(':scope > summary').click();
    }

    const searchRow = disclosure.locator(
      '[data-step-id="step_1"]',
    );
    await expect(searchRow).toHaveAttribute('data-step-status', 'ok');
    await expect(searchRow.getByTestId('chat-agent-step-tool')).toHaveText(
      'web_search',
    );

    const fetchRow = disclosure.locator('[data-step-id="step_2"]');
    await expect(fetchRow).toHaveAttribute('data-step-status', 'ok');
    await expect(fetchRow.getByTestId('chat-agent-step-tool')).toHaveText(
      'web_fetch',
    );

    // The search args carry the query the model emitted.
    await searchRow
      .getByTestId('chat-agent-step-args')
      .locator(':scope > summary')
      .click();
    await expect(searchRow.getByTestId('chat-agent-step-args')).toContainText(
      'nyx agent',
    );

    // Tool results render as escaped text: the untrusted-content payload is
    // visible as JSON text, and NO <untrusted_web_content> element can exist.
    await expect(searchRow.getByTestId('chat-agent-step-result')).toContainText(
      'results',
    );
    await expect(fetchRow.getByTestId('chat-agent-step-result')).toContainText(
      'text/html',
    );
    await expect(page.locator('untrusted_web_content')).toHaveCount(0);

    // The answering step's text streamed to the assistant bubble.
    const assistant = page.getByTestId('chat-message-assistant').last();
    await expect(assistant).toContainText('ordered steps', {
      timeout: 5000,
    });

    // Clean run: no partial-failure banner, no failed steps.
    await expect(page.getByTestId('chat-partial-failure-banner')).toHaveCount(
      0,
    );
  });
});
