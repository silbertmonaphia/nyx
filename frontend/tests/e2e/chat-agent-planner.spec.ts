import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Planner tool-step path — browser verification for `agent-planner-structured`.
 *
 * chat-rag.spec.ts covers the llm-step happy path; this spec targets the
 * planner's tool-step contract beyond it:
 *   - a plan containing a `tool` step renders its row in the agent-steps
 *     disclosure, with the tool name from the `tool_call` frame;
 *   - every step row reaches a terminal status (`ok`/`failed`) — no row
 *     left spinning after `done`. Note the disclosure auto-expands while
 *     steps run and auto-collapses on settle (`<details open={hasRunning}>`
 *     in ChatPanel.tsx); the spec re-expands it post-settle — the same
 *     click a user makes to inspect a finished trace;
 *   - the final assistant reply is non-empty (the appended answering step
 *     worked) and no error frame surfaced.
 *
 * Assumes the dev stack is up: `make up` (db/redis/jaeger) + `make dev`
 * (backend :8080, frontend :5173, Python agent :9000) with a reachable LLM
 * provider. playwright.config.ts only manages the frontend dev server; the
 * others are preconditions.
 *
 * Flake posture: the planner is a live model. The prompt below instructs the
 * tool explicitly; if the configured model still declines to emit the tool
 * step, this spec fails — that is the intent (the plan-shape contract is what
 * the gate verifies). Loosen only with recorded evidence.
 *
 * A second describe replays recorded failure-path wire bytes (fixtures/)
 * through the frontend's own SSE parser via route fulfillment — the
 * retry-recovered and retry-exhausted streams produced by the §2.2 gate
 * run against `openspec/changes/agent-planner-structured-rollout/probe/
 * stub_provider.py`. No live model involved; proves the UI renders both
 * paths without a crashed stream.
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
    username: `e2e_planner_${Date.now()}_${Math.floor(Math.random() * 100000)}`,
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

test.describe('Chat / planner tool-step path (browser-level)', { tag: '@live' }, () => {
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page);
  });

  test('a tool-step plan renders the step trace and a final answer', async ({
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

    // No error frame on the happy path.
    await expect(page.getByTestId('chat-error')).toHaveCount(0);

    // The agent-steps disclosure rendered. It is expanded while steps run
    // and auto-collapses when the last step reaches a terminal status
    // (`<details open={m.steps.some(s => s.status === "running")}>`), so
    // after settle re-expand it — the click a user makes to inspect a
    // finished trace. The rows stay in the DOM either way.
    const stepsDisclosure = page.getByTestId('chat-agent-steps').last();
    await expect(stepsDisclosure).toBeVisible();
    const isOpen = await stepsDisclosure.evaluate(
      (el) => (el as HTMLDetailsElement).open,
    );
    if (!isOpen) {
      // `:scope > summary` — the disclosure's OWN summary only. A bare
      // `summary` descendant query also matches each tool row's nested
      // `args` `<details><summary>` (StepRow), and `.click()` then trips
      // Playwright strict mode ("resolved to 2 elements").
      await stepsDisclosure.locator(':scope > summary').click();
    }
    await expect(stepsDisclosure).toHaveAttribute('open', '', {
      timeout: 5000,
    });

    const stepRows = stepsDisclosure.getByTestId('chat-agent-step');
    expect(await stepRows.count()).toBeGreaterThan(0);

    // The `tool` step for list_feeds is present — i.e. the tool_call frame
    // rendered its row with the tool name.
    await expect(
      stepsDisclosure
        .getByTestId('chat-agent-step-tool')
        .filter({ hasText: 'list_feeds' })
        .first(),
    ).toBeVisible({ timeout: 10000 });

    // Every step row reached a terminal status — nothing left spinning.
    for (const row of await stepRows.all()) {
      const status = await row.getAttribute('data-step-status');
      expect(['ok', 'failed']).toContain(status);
    }

    // The final assistant reply is non-empty (the appended answering step
    // produced visible text).
    const assistant = page.getByTestId('chat-message-assistant').last();
    await expect(assistant).not.toHaveText(/^\s*$/, { timeout: 10000 });
  });
});

test.describe('Chat / planner failure frames (recorded wire bytes)', () => {
  // Fixtures are SSE captures from the §2.2 gate run: a real agent
  // process (structured planner enabled) pointed at probe/stub_provider.py,
  // which ignores `response_format` — planner call #1 malformed, the retry
  // request carries PREVIOUS_ATTEMPT_FAILED.
  //   - planner-retry-recovered.sse: call #2 valid → run completes.
  //   - planner-retry-exhausted.sse: every call malformed → typed error.
  //     Annotated post-capture (agent-tool-dispatch-retry, review round 2):
  //     a `note{kind:"partial_failure"}` frame now sits between error and
  //     done, matching the current producer (planner `_emit_error`).
  // Replaying them through the frontend's real SSE parser + render path
  // covers "user-visible chat completes via the retry path, or fails with
  // a typed error — never a crashed stream" (rollout gate §2.2).
  test.beforeEach(async ({ page }) => {
    await registerAndOpenChat(page);
  });

  test('retry-recovered planner stream renders the trace and answer', async ({
    page,
  }) => {
    await page.route('**/api/chat*', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: fixture('planner-retry-recovered.sse'),
      }),
    );

    await page.getByTestId('chat-input').fill('hi');
    await page.getByTestId('chat-send').click();

    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 15000,
    });
    await expect(page.getByTestId('chat-error')).toHaveCount(0);

    const disclosure = page.getByTestId('chat-agent-steps').last();
    await expect(disclosure).toBeVisible();
    const isOpen = await disclosure.evaluate(
      (el) => (el as HTMLDetailsElement).open,
    );
    if (!isOpen) {
      await disclosure.locator(':scope > summary').click();
    }
    const rows = disclosure.getByTestId('chat-agent-step');
    expect(await rows.count()).toBeGreaterThan(0);
    for (const row of await rows.all()) {
      const status = await row.getAttribute('data-step-status');
      expect(['ok', 'failed']).toContain(status);
    }

    const assistant = page.getByTestId('chat-message-assistant').last();
    await expect(assistant).not.toHaveText(/^\s*$/, { timeout: 5000 });
  });

  test('exhausted planner stream surfaces the typed error, never a crash', async ({
    page,
  }) => {
    await page.route('**/api/chat*', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'text/event-stream',
        body: fixture('planner-retry-exhausted.sse'),
      }),
    );

    await page.getByTestId('chat-input').fill('hi');
    await page.getByTestId('chat-send').click();

    // The stream still terminates cleanly (the cancel button unmounts) …
    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 15000,
    });
    // … the typed error surfaces as user-visible text, and no step trace
    // rendered (no `plan` frame ever arrived — the failure was terminal).
    await expect(page.getByTestId('chat-error')).toBeVisible();
    await expect(page.getByTestId('chat-error')).toContainText(
      'Plan is invalid',
    );
    await expect(page.getByTestId('chat-agent-steps')).toHaveCount(0);
  });
});
