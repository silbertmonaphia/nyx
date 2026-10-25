import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { registerAndOpenChat as registerAndOpenChatShared } from './helpers/llm-config';

/**
 * Background runs — browser verification for `agent-chat-background-runs`.
 *
 * The behavior under test: navigating away from a streaming conversation
 * (`+ New chat`, a session switch, closing the dialog) must NOT kill the
 * run. The SPA detaches the stream (the SSE connection stays open — it is
 * the run's liveness signal; aborting it cancels the agent server-side),
 * drops its frames, marks the run's rail row with a "still working" pulse,
 * and refreshes/replays once the run reaches its terminal state.
 *
 * Exercises:
 *   - `+ New chat` mid-stream: panel clears, the old row keeps pulsing, the
 *     fresh composer is immediately usable;
 *   - the pulse clears when the run finishes and the finished conversation
 *     replays in full (user prompt + assistant answer persisted by the
 *     backgrounded run);
 *   - revisiting the still-running session: the composer is locked with an
 *     in-progress hint (a second concurrent run on one session would
 *     interleave its rows), then unlocks with the replayed answer.
 *
 * Live-backend dependency (why this spec is not runnable in CI alone):
 * every test drives a real `POST /api/chat` SSE run — `AGENT_ENABLED=true`,
 * the Python agent up, and a reachable LLM provider are required, and the
 * prompts ask for long outputs so the run is still streaming when the
 * navigation lands. Same preconditions as chat-history-sidebar.spec.ts:
 * `make up` (db/redis/jaeger) + backend + agent + vLLM. To avoid touching
 * an operator-owned dev stack, drive the side stack instead:
 * `PLAYWRIGHT_BASE_URL=http://localhost:5273 npx playwright test
 * tests/e2e/chat-background-runs.spec.ts --workers=1`.
 *
 * Run with `--workers=1`: the shared 4B vLLM serializes under parallel
 * workers and live specs flake (documented shared-GPU contention).
 */

const CURRENT_SESSION_KEY = 'nyx.chat.currentSessionId';

async function registerAndOpenChat(page: Page): Promise<void> {
  await registerAndOpenChatShared(page, { prefix: 'e2e_bgruns' });
}

/**
 * Send a prompt and return as soon as the stream has visibly started —
 * deliberately NOT waiting for completion (the `sendMessage` helper in
 * chat-history-sidebar.spec.ts settles the run; these tests need it live).
 * The long "count to N" prompts keep the run streaming for several seconds
 * after the first delta, which is what gives the navigation something to
 * detach from.
 */
async function startStreamingMessage(page: Page, text: string): Promise<void> {
  await page.getByTestId('chat-input').fill(text);
  await page.getByTestId('chat-send').click();
  await expect(page.getByTestId('chat-message-assistant').last()).not.toHaveText(
    /^[\s…]*$/u,
    { timeout: 45000 },
  );
  // The run's session is adopted (and persisted) at the `run_id` frame —
  // the first frame of the run, before any delta reaches the DOM.
  await expect
    .poll(() => persistedSessionId(page), { timeout: 15000 })
    .not.toBeNull();
}

async function persistedSessionId(page: Page): Promise<string | null> {
  return page.evaluate(
    (key) => window.sessionStorage.getItem(key),
    CURRENT_SESSION_KEY,
  );
}

async function waitForPersistedSessionIdChange(
  page: Page,
  previous: string | null,
): Promise<string> {
  // `+ New chat` clears the persisted id synchronously and persists the
  // minted one only after `POST /api/chat/sessions` resolves — so poll
  // for a NON-NULL value that differs from the previous one; a bare
  // "changed" check would pass through the null window.
  await expect
    .poll(
      async () => {
        const id = await persistedSessionId(page);
        return id !== null && id !== previous;
      },
      { timeout: 15000 },
    )
    .toBe(true);
  const id = await persistedSessionId(page);
  if (id === null || id === previous) {
    throw new Error('session id was not persisted after + New chat');
  }
  return id;
}

function rowFor(page: Page, sessionId: string) {
  return page.locator(
    `[data-testid="chat-session-row"][data-session-id="${sessionId}"]`,
  );
}

test.describe('Chat background runs (browser-level, live backend)', { tag: '@live' }, () => {
  test('New chat mid-stream: the run survives, pulses in the rail, and replays in full', async ({
    page,
  }) => {
    await registerAndOpenChat(page);

    const marker = `bg-${Date.now()}`;
    const prompt = `Count from 1 to 60, one number per line, nothing else. (${marker})`;
    await startStreamingMessage(page, prompt);
    const oldSessionId = await persistedSessionId(page);
    expect(oldSessionId).not.toBeNull();

    // Leave the run behind mid-answer. Assert the rail pulse FIRST — it
    // is the freshest signal and the run is still streaming.
    await page.getByTestId('chat-new-session').click();
    const oldRow = rowFor(page, oldSessionId!);
    await expect(oldRow.getByTestId('chat-session-running')).toBeVisible({
      timeout: 15000,
    });

    // The panel moved on: welcome state, no live cancel affordance…
    await expect(page.getByText(/No messages yet/)).toBeVisible();
    await expect(page.getByTestId('chat-message-user')).toHaveCount(0);
    await expect(page.getByTestId('chat-cancel')).toHaveCount(0);
    // …and the fresh session is immediately usable — that is the point of
    // putting the old run in the background.
    await expect(page.getByTestId('chat-input')).not.toBeDisabled();
    await waitForPersistedSessionIdChange(page, oldSessionId);

    // The backgrounded run finishes: the pulse clears.
    await expect(oldRow.getByTestId('chat-session-running')).toHaveCount(0, {
      timeout: 90000,
    });

    // The finished conversation replays in full — including the assistant
    // answer the run persisted AFTER the SPA left it. A user-row-only
    // replay means the detach killed the run instead of backgrounding it.
    await oldRow.click();
    await expect(page.getByTestId('chat-message-user')).toHaveText(prompt, {
      timeout: 30000,
    });
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).not.toHaveText(/^[\s…]*$/u, { timeout: 30000 });
    await expect(page.getByTestId('chat-input')).not.toBeDisabled();
  });

  test('revisiting the still-running session locks the composer until the answer lands', async ({
    page,
  }) => {
    await registerAndOpenChat(page);

    const prompt = `Count from 1 to 60, one number per line, nothing else. (${Date.now()}-busy)`;
    await startStreamingMessage(page, prompt);
    const busySessionId = await persistedSessionId(page);
    expect(busySessionId).not.toBeNull();

    // Move to a fresh chat, then immediately back into the busy one while
    // the run is still streaming.
    await page.getByTestId('chat-new-session').click();
    await waitForPersistedSessionIdChange(page, busySessionId);
    const busyRow = rowFor(page, busySessionId!);
    await expect(busyRow.getByTestId('chat-session-running')).toBeVisible({
      timeout: 15000,
    });

    await busyRow.click();
    // The persisted rows replay as they are mid-run: the user row only.
    await expect(page.getByTestId('chat-message-user')).toHaveText(prompt, {
      timeout: 15000,
    });
    // The composer is locked with the in-progress hint: a second
    // concurrent run on one session would interleave its rows.
    await expect(page.getByTestId('chat-session-busy-hint')).toBeVisible({
      timeout: 15000,
    });
    await expect(page.getByTestId('chat-input')).toBeDisabled();
    await expect(page.getByTestId('chat-send')).toBeDisabled();

    // The run's terminal frame replays the completed session: hint gone,
    // answer present, composer unlocked.
    await expect(page.getByTestId('chat-session-busy-hint')).toHaveCount(0, {
      timeout: 90000,
    });
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).not.toHaveText(/^[\s…]*$/u, { timeout: 30000 });
    await expect(page.getByTestId('chat-input')).not.toBeDisabled();
  });

  test('closing the dialog mid-stream keeps the run alive', async ({ page }) => {
    await registerAndOpenChat(page);

    const prompt = `Count from 1 to 60, one number per line, nothing else. (${Date.now()}-close)`;
    await startStreamingMessage(page, prompt);
    const sessionId = await persistedSessionId(page);
    expect(sessionId).not.toBeNull();

    // Escape is the close gesture — its reset() must detach, not abort.
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('chat-form')).toHaveCount(0);

    // Reopening restores the session; the run is still working (rail
    // pulse + locked composer while its replay shows the user row).
    await page.getByRole('button', { name: 'Open AI Agent' }).click();
    await expect(page.getByTestId('chat-form')).toBeVisible();
    const row = rowFor(page, sessionId!);
    await expect(row.getByTestId('chat-session-running')).toBeVisible({
      timeout: 15000,
    });
    await expect(page.getByTestId('chat-session-busy-hint')).toBeVisible({
      timeout: 15000,
    });

    // Terminal frame: the completed turn replays and the composer unlocks.
    await expect(page.getByTestId('chat-session-busy-hint')).toHaveCount(0, {
      timeout: 90000,
    });
    await expect(page.getByTestId('chat-message-user')).toHaveText(prompt, {
      timeout: 30000,
    });
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).not.toHaveText(/^[\s…]*$/u, { timeout: 30000 });
    await expect(page.getByTestId('chat-input')).not.toBeDisabled();
  });
});
