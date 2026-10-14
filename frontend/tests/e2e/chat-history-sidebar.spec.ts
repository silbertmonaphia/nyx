import { test, expect, type Page } from '@playwright/test';

/**
 * Chat history sidebar — browser verification for `agent-chat-history-ui`.
 *
 * Exercises the SPA-visible half of the change:
 *   - the rail lists the tab's sessions newest-first, with a placeholder
 *     label for untitled sessions and a relative timestamp;
 *   - a reload restores the active session from sessionStorage and
 *     replays its stored turns into the panel;
 *   - `+ New chat` clears the panel but keeps the rail rows; the next
 *     message is recorded under a fresh session id;
 *   - clicking an older row replays it (chronological bubbles) and
 *     clicking the active row is a no-op (no refetch);
 *   - hover+delete drops a row (and clears the panel when it was the
 *     active one);
 *   - below `md` the rail is a drawer: hidden by default, opened by the
 *     hamburger, closed again by picking a session.
 *
 * Live-backend dependency (why this spec is not runnable in CI alone):
 * every test needs the auth endpoints plus the Slice 1 session REST
 * endpoints proxied through the Go API — `GET/POST /api/chat/sessions`,
 * `GET /api/chat/sessions/{id}/messages`, `POST .../{id}/resume`,
 * `DELETE .../{id}`. The three tests that send a message additionally
 * need the full `POST /api/chat` SSE path: `AGENT_ENABLED=true`, the
 * Python agent up, and a reachable LLM provider. The delete and drawer
 * tests deliberately avoid the LLM — they mint their session with
 * `+ New chat`, which only needs `POST /api/chat/sessions`, and assume
 * that list endpoint returns rows with `message_count: 0` (a brand-new
 * session must be visible to its owner, or the rail lies about the
 * session it just created).
 *
 * Preconditions, same as chat-agent-planner.spec.ts: `make up`
 * (db/redis/jaeger) + `make dev` (backend :8080, frontend :5173, Python
 * agent :9000). playwright.config.ts owns the frontend dev server; the
 * rest are operator-provided.
 */

// Fresh user per test — see chat-rag.spec.ts `makeTestUser` for why a
// module-level constant re-registered in beforeEach collides (409).
function makeTestUser() {
  return {
    username: `e2e_history_${Date.now()}_${Math.floor(Math.random() * 100000)}`,
    password: 'testpass123',
  };
}

const CURRENT_SESSION_KEY = 'nyx.chat.currentSessionId';

async function registerAndOpenChat(page: Page): Promise<void> {
  const user = makeTestUser();
  await page.goto('/');
  await page.getByRole('button', { name: /Login \/ Register/i }).click();
  await page.getByRole('button', { name: /Register/i }).last().click();
  await page.getByLabel(/Username/i).fill(user.username);
  await page.getByLabel(/Password/i).fill(user.password);
  await page.getByRole('button', { name: /^Register$/i }).click();
  await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
  await page.getByRole('button', { name: 'Open chat' }).click();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

/**
 * A reload drops the dialog's open flag (component state, not
 * persisted) — reopen it. The rail is the same DOM either way; only the
 * drawer class differs across the breakpoint.
 */
async function reopenChat(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Open chat' }).click();
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

/**
 * Send one turn and wait for the stream to settle.
 *
 * The settle gate is two-step on purpose: waiting only for the Cancel
 * button to disappear passes vacuously when the assertion wins the race
 * against the first delta. Requiring non-empty assistant text first
 * proves the stream started, so the later `chat-cancel` count of 0 is
 * meaningful.
 */
async function sendMessage(page: Page, text: string): Promise<void> {
  await page.getByTestId('chat-input').fill(text);
  await page.getByTestId('chat-send').click();
  // `/^[\s…]*$/` — the bubble renders the "…" placeholder while
  // streaming with no content yet; a bare `/^\s*$/` would pass on that
  // placeholder and make the "stream started" gate vacuous.
  await expect(page.getByTestId('chat-message-assistant').last()).not.toHaveText(
    /^[\s…]*$/u,
    { timeout: 45000 },
  );
  await expect(page.getByTestId('chat-cancel')).toHaveCount(0, {
    timeout: 45000,
  });
  await expect(page.getByTestId('chat-error')).toHaveCount(0);
}

/** Session id the store persisted for this tab (null when none). */
async function persistedSessionId(page: Page): Promise<string | null> {
  return page.evaluate(
    (key) => window.sessionStorage.getItem(key),
    CURRENT_SESSION_KEY,
  );
}

/**
 * `+ New chat` persists its id only AFTER `POST /api/chat/sessions`
 * resolves — reading sessionStorage immediately after the click races
 * the round-trip (observed: null on the first read). Poll instead.
 */
async function waitForPersistedSessionId(page: Page): Promise<string> {
  await expect
    .poll(() => persistedSessionId(page), { timeout: 15000 })
    .not.toBeNull();
  const id = await persistedSessionId(page);
  if (id === null) {
    throw new Error('session id was not persisted after + New chat');
  }
  return id;
}

/** Rail row ids in render order (the server owns the sort). */
async function railRowIds(page: Page): Promise<string[]> {
  return page
    .getByTestId('chat-session-row')
    .evaluateAll((rows) =>
      rows.map((row) => row.getAttribute('data-session-id') ?? ''),
    );
}

test.describe('Chat history sidebar (browser-level, live backend)', () => {
  test('a new account starts with an empty rail', async ({ page }) => {
    await registerAndOpenChat(page);

    await expect(page.getByTestId('chat-sidebar')).toBeVisible();
    await expect(page.getByTestId('chat-session-empty')).toHaveText(
      'No conversations yet',
    );
    expect(await railRowIds(page)).toEqual([]);
    expect(await persistedSessionId(page)).toBeNull();
  });

  test('a reload restores the active session and replays its messages', async ({
    page,
  }) => {
    await registerAndOpenChat(page);

    const marker = `history-${Date.now()}`;
    const prompt = `Reply with one short sentence. (${marker})`;
    await sendMessage(page, prompt);

    // The first run mints the session server-side and the store adopts
    // the run id from the SSE frame.
    const sessionId = await persistedSessionId(page);
    expect(sessionId).not.toBeNull();
    const row = page.locator('[data-testid="chat-session-row"]');
    await expect(row).toHaveCount(1);
    await expect(row).toHaveAttribute('data-active', 'true');
    await expect(row).toHaveAttribute('data-session-id', sessionId!);

    // Reload: the rail refetches page 1 and the panel replays the
    // stored turn (spec "Reload restores the active session").
    await page.reload();
    await reopenChat(page);

    await expect(page.getByTestId('chat-message-user')).toHaveText(prompt, {
      timeout: 15000,
    });
    // The assistant reply must replay too: a user-only replay means the
    // turn's terminal persistence was lost (the client-disconnect race
    // the finalize shield in agent/main.py closes).
    await expect(
      page.getByTestId('chat-message-assistant').last(),
    ).not.toHaveText(/^\s*$/, { timeout: 15000 });
    await expect(page.locator('[data-testid="chat-session-row"]')).toHaveAttribute(
      'data-active',
      'true',
    );
    expect(await persistedSessionId(page)).toBe(sessionId);
  });

  test('New chat clears the panel, keeps the rail, and the next turn lands in a new session', async ({
    page,
  }) => {
    await registerAndOpenChat(page);

    const firstPrompt = `First conversation. (${Date.now()}-a)`;
    await sendMessage(page, firstPrompt);
    const firstSessionId = await persistedSessionId(page);
    expect(firstSessionId).not.toBeNull();

    // `+ New chat` — panel flips to the welcome state, no row is active,
    // the existing row stays put (spec Scenarios "New chat clears the
    // panel" + "New chat keeps the sidebar list").
    await page.getByTestId('chat-new-session').click();
    await expect(page.getByText(/No messages yet/)).toBeVisible();
    await expect(page.getByTestId('chat-message-user')).toHaveCount(0);
    await expect(page.locator('[data-testid="chat-session-row"]')).toHaveCount(1);
    await expect(
      page.locator('[data-testid="chat-session-row"][data-active="true"]'),
    ).toHaveCount(0);

    // The freshly minted session id is active (persisted) but not yet in
    // the stale list snapshot — the rail only refetches on mount.
    const newSessionId = await waitForPersistedSessionId(page);
    expect(newSessionId).not.toBe(firstSessionId);

    const secondPrompt = `Second conversation. (${Date.now()}-b)`;
    await sendMessage(page, secondPrompt);

    // Reload: two rows, newest first, the second one active and holding
    // the second conversation's messages.
    await page.reload();
    await reopenChat(page);

    await expect(page.getByTestId('chat-message-user')).toHaveText(secondPrompt, {
      timeout: 15000,
    });
    await expect(page.getByTestId('chat-session-row')).toHaveCount(2);
    expect(await railRowIds(page)).toEqual([newSessionId, firstSessionId]);
    await expect(
      page.locator('[data-testid="chat-session-row"][data-active="true"]'),
    ).toHaveAttribute('data-session-id', newSessionId!);
  });

  test('clicking an older session replays it; clicking the active row is a no-op', async ({
    page,
  }) => {
    await registerAndOpenChat(page);

    // The request log is how the no-op is proven: the second click must
    // not hit the messages endpoint again (spec Scenario "Clicking the
    // active session is a no-op").
    const replayRequests: string[] = [];
    page.on('request', (request) => {
      const { pathname } = new URL(request.url());
      if (/\/api\/chat\/sessions\/[^/]+\/messages$/.test(pathname)) {
        replayRequests.push(pathname);
      }
    });

    const olderPrompt = `Oldest conversation. (${Date.now()}-old)`;
    await sendMessage(page, olderPrompt);
    const olderSessionId = await persistedSessionId(page);

    await page.getByTestId('chat-new-session').click();
    const newerPrompt = `Newest conversation. (${Date.now()}-new)`;
    await sendMessage(page, newerPrompt);
    const newerSessionId = await persistedSessionId(page);

    await page.reload();
    await reopenChat(page);
    await expect(page.getByTestId('chat-session-row')).toHaveCount(2);
    // Both conversations are in the rail, newest first.
    await expect(
      page.locator(
        `[data-testid="chat-session-row"][data-session-id="${newerSessionId}"]`,
      ),
    ).toBeVisible();

    const callsBefore = replayRequests.length;
    const olderRow = page.locator(
      `[data-testid="chat-session-row"][data-session-id="${olderSessionId}"]`,
    );
    await olderRow.click();
    await expect(page.getByTestId('chat-message-user')).toHaveText(olderPrompt, {
      timeout: 15000,
    });
    expect(await persistedSessionId(page)).toBe(olderSessionId);
    expect(replayRequests.length).toBeGreaterThan(callsBefore);

    // Re-click the now-active row → nothing refetches, the panel keeps
    // the replayed history.
    await olderRow.click();
    await expect(page.getByTestId('chat-message-user')).toHaveText(olderPrompt);
    expect(replayRequests.length).toBe(callsBefore + 1);
    expect(await persistedSessionId(page)).toBe(olderSessionId);
    // The other conversation's text never leaked into the panel.
    await expect(page.getByTestId('chat-message-list')).not.toContainText(
      newerPrompt,
    );
  });

  test('deleting the active session drops the row and clears the panel', async ({
    page,
  }) => {
    await registerAndOpenChat(page);

    // No LLM needed: `+ New chat` mints a real (empty) session row.
    await page.getByTestId('chat-new-session').click();
    const sessionId = await waitForPersistedSessionId(page);

    await page.reload();
    await reopenChat(page);

    const row = page.locator(
      `[data-testid="chat-session-row"][data-session-id="${sessionId}"]`,
    );
    await expect(row).toBeVisible({ timeout: 15000 });
    // Untitled session → placeholder label.
    await expect(row.getByTestId('chat-session-title')).toHaveText('New chat');

    // The affordance is hover/focus-revealed, so drive it like a mouse.
    // The delete button is a SIBLING of the row button (both inside the
    // row's <li>), so it must be looked up from the page, joined by the
    // shared data-session-id.
    await row.hover();
    await page
      .locator(
        `[data-testid="chat-session-delete"][data-session-id="${sessionId}"]`,
      )
      .click();

    await expect(row).toHaveCount(0);
    // It was the active session — the panel returns to the welcome state
    // and the tab forgets the id, so a reload stays graceful.
    await expect(page.getByText(/No messages yet/)).toBeVisible();
    expect(await persistedSessionId(page)).toBeNull();
  });

  test('narrow viewport: the hamburger drawer opens and a session click closes it', async ({
    page,
  }) => {
    // 600px sits below the `md` (768px) breakpoint the drawer hangs off.
    await page.setViewportSize({ width: 600, height: 900 });
    await registerAndOpenChat(page);

    // No LLM needed: mint the row the drawer will hold.
    await page.getByTestId('chat-new-session').click();
    const sessionId = await waitForPersistedSessionId(page);
    await page.reload();
    await reopenChat(page);

    // Hidden by default (Tailwind `hidden` = display:none, which
    // Playwright reports as not visible) …
    const toggle = page.getByTestId('chat-sidebar-toggle');
    await expect(toggle).toBeVisible();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await expect(page.getByTestId('chat-sidebar')).not.toBeVisible();

    // … the hamburger opens it as an overlay …
    await toggle.click();
    await expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await expect(page.getByTestId('chat-sidebar')).toBeVisible();

    // … and picking a session closes it again.
    await page
      .locator(`[data-testid="chat-session-row"][data-session-id="${sessionId}"]`)
      .click();
    await expect(page.getByTestId('chat-sidebar')).not.toBeVisible();
    await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  });
});
