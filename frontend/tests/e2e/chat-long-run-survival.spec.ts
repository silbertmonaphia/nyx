import { expect, type Page, type APIRequestContext } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { llmConfigSeedBody } from './helpers/llm-config';

/**
 * Long-run survival + the summarize→create journey (live stack).
 *
 * Guards the 2026-10-14 incident. The operator asked the chat:
 * 帮我总结我最近3个月的feed，然后把总结的内容发一篇新的feed
 * The run planned a react step with no objective (surfacing
 * "ReAct step exceeded iteration cap"), then paused on the create_feed
 * approval card — and the Go chat proxy's `&http.Client{Timeout: 60 * s}`
 * severed the stream at t+61.8s. http.Client.Timeout bounds the ENTIRE
 * exchange including the streamed body, so the wire went from keep-alive
 * comments to a dead connection: no done, no error, no terminator, the
 * run cancelled server-side (Python's pump hangs off the request
 * context), the approval orphaned, and no assistant row persisted.
 *
 * Two fixes ride this spec, both only observable against the live stack:
 *
 *   1. The proxy client carries no Timeout; the upstream stream is
 *      bounded by LLM_MAX_STREAM_DURATION (default 10m) on the
 *      request-derived context, and a cap cut emits typed
 *      error{code:"stream_timeout"} + done + terminator instead of a
 *      silent death. The 70s hold below outlives the old 60s cap — on
 *      the old code the approve click then completes nothing and the
 *      feed never lands.
 *   2. React steps run with a synthesized objective + stop rule when
 *      the planner omits `prompt` (the shape its schema advertised).
 *      A wander-until-cap run surfaces as a react_iterations_exceeded
 *      error frame — asserted absent.
 *
 * Environment: PLAYWRIGHT_BASE_URL must point at a side stack running
 * THIS worktree's code (`E2E_GO_PORT=8181 E2E_AGENT_PORT=9102
 * E2E_AGENT_METRICS_PORT=9103 E2E_VITE_PORT=5274 E2E_STACK_SUFFIX=-rc1
 * ./scripts/e2e-side-stack.sh up`), with MCP_ENABLED=true and a
 * reachable LLM (host vLLM on :8000). The seed API base defaults to the
 * side-stack script's default Go port; override with E2E_GO_PORT when
 * running on suffixed ports. Serial workers only — one 4B vLLM serves
 * every live spec.
 *
 * Side effects: one throwaway user + three seeded feeds + one created feed.
 */

const GO_PORT = process.env.E2E_GO_PORT ?? '8180';
const API = `http://127.0.0.1:${GO_PORT}/api`;

// The operator's exact prompt — the journey this spec exists to keep green.
const JOURNEY_PROMPT =
  '帮我总结我最近3个月的feed，然后把总结的内容发一篇新的feed';

function makeTestUser() {
  return {
    username: `e2e_longrun_${Date.now()}_${Math.floor(Math.random() * 100000)}`,
    password: 'testpass123',
  };
}

/**
 * Register via the API (so the same account can seed feeds), then log in
 * through the real auth form so the SPA owns the session exactly as a
 * human's would.
 */
async function registerSeedAndLogin(
  page: Page,
  request: APIRequestContext,
): Promise<void> {
  const user = makeTestUser();

  const reg = await request.post(`${API}/register`, { data: user });
  expect(
    reg.status(),
    `register failed: ${reg.status()} ${await reg.text()}`,
  ).toBe(201);
  const { access_token } = (await reg.json()) as { access_token: string };

  const seededConfig = await request.put(`${API}/me/llm-config`, {
    headers: { Authorization: `Bearer ${access_token}` },
    data: llmConfigSeedBody(),
  });
  expect(
    seededConfig.ok(),
    `llm config seed failed: ${seededConfig.status()} ${await seededConfig.text()}`,
  ).toBeTruthy();

  // Mirror the operator's data shape: three recent feeds, no query match
  // for the planner's args — the plan must still summarize + create.
  for (const title of ['好冷', '三点一线', '又是失眠的一天']) {
    const seeded = await request.post(`${API}/feeds`, {
      headers: { Authorization: `Bearer ${access_token}` },
      data: { title, description: `${title} 的一天` },
    });
    expect(seeded.ok(), `seed feed ${title} failed`).toBeTruthy();
  }

  await page.goto('/');
  await page.getByLabel(/Username/i).fill(user.username);
  await page.getByLabel(/Password/i).fill(user.password);
  await page.getByRole('button', { name: /^Login$/i }).click();
  await expect(page.getByText(/Successfully logged in!/i)).toBeVisible();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

test.describe('Long agent runs survive the stream (live stack)', { tag: '@live' }, () => {
  test('summarize→create completes after a >60s approval hold', async ({
    page,
    request,
  }) => {
    // Internal waits can sum past 350s on a slow planner roll
    // (120s card + 70s hold + 30s approve + 120s settle + 15s list);
    // 420s keeps a slow run a signal failure, not a budget failure.
    test.setTimeout(420_000);

    await registerSeedAndLogin(page, request);

    await page.getByTestId('chat-input').fill(JOURNEY_PROMPT);
    await page.getByTestId('chat-send').click();

    // The run plans → summarizes → pauses on the create_feed card. The
    // live planner is a small model; generous window, same posture as
    // chat-feed-approval-live.spec.ts.
    const card = page.getByTestId('chat-confirm-card');
    await expect(card).toBeVisible({ timeout: 120_000 });
    await expect(page.getByTestId('chat-confirm-tool')).toHaveText('create_feed');

    // Let the run outlive the OLD 60s cap (measured from request start)
    // before deciding. On the old proxy client the stream is already dead
    // here — the approve below would flip the card but complete nothing.
    await page.waitForTimeout(70_000);

    // The stream must still be live: the card is still pending, and no
    // error frame arrived (a react step that wandered to its cap would
    // have emitted react_iterations_exceeded by now).
    await expect(card).toBeVisible();
    await expect(card).toHaveAttribute('data-decision', 'pending');
    await expect(page.getByTestId('chat-error')).toHaveCount(0);

    // Capture the title the model chose, so the feed-landed assertion
    // does not need to predict the planner's wording.
    const argsText = (await page.getByTestId('chat-confirm-args').textContent()) ?? '';
    const titleMatch = argsText.match(/"title"\s*:\s*"([^"]+)"/);
    expect(titleMatch, `confirm args must carry a title; got: ${argsText}`).toBeTruthy();
    const createdTitle = titleMatch![1];

    await page.getByTestId('chat-confirm-approve').click();
    await expect(card).toHaveAttribute('data-decision', 'approved', {
      timeout: 30_000,
    });

    // The run resumes on the SAME connection and settles — the approved
    // call executes and the final answer streams.
    await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
      timeout: 120_000,
    });
    await expect(page.getByTestId('chat-error')).toHaveCount(0);

    // Close the chat and reload: the feeds page shows the created row —
    // proof the resume decision executed against the real registry +
    // Postgres, not just against UI state.
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('chat-form')).not.toBeVisible();
    await page.reload();
    await expect(page.getByTestId('feed-list')).toContainText(createdTitle, {
      timeout: 15_000,
    });
  });
});
