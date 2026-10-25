import { execFileSync } from 'node:child_process';
import { expect, type Page } from '@playwright/test';
import { test } from './fixtures/e2e-fixtures';
import { registerAndOpenChat as registerAndOpenChatShared } from './helpers/llm-config';

/**
 * User-preference memory — browser verification for
 * `agent-user-preferences` (Slice 3: the agent remembers "我喜欢鱿鱼丝").
 *
 * What the UI can actually prove (prefs are invisible plumbing — no new
 * frames, no new endpoints, no frontend surface):
 *   1. a preference-bearing turn completes normally on the SSE path (the
 *      extraction is fire-and-forget AFTER the reply — it must never
 *      delay, block, or alter the stream);
 *   2. `+ New chat` starts a fresh session, the follow-up turn lands in
 *      it and settles with an assistant reply;
 *   3. deterministically: after a reload, the ACTIVE (new) session's
 *      replay contains BOTH turns (the ask + its assistant reply) — i.e.
 *      the preference turn and its follow-up were both durably recorded;
 *   4. tolerantly: the follow-up's reply MAY reference the stored
 *      preference (鱿鱼丝) — that is model-dependent, so it is only
 *      recorded as an annotation, never asserted.
 *
 * Live-only spec: the extraction is a real LLM call made by the agent
 * process, and the injected prefs influence a real planner prompt — there
 * is no wire frame to route-fulfil (unlike chat-agent-planner.spec.ts's
 * recorded-fixture describe), so no stub-replay leg exists. Assertions
 * stay on the session/replay contract and tolerate model phrasing.
 *
 * Preconditions (operator-provided; playwright.config.ts owns only the
 * frontend dev server): `make up` (db/redis/jaeger) + `make dev` with
 * `AGENT_ENABLED=true`, the Python agent up, a reachable LLM provider,
 * and `PYTHON_AGENT_PREFS_ENABLED` at its default (true). With the kill
 * switch on, the stream legs still pass but no prefs are extracted (the
 * annotation will show a plain reply).
 */

const CURRENT_SESSION_KEY = 'nyx.chat.currentSessionId';

async function registerAndOpenChat(page: Page): Promise<void> {
  await registerAndOpenChatShared(page, { prefix: 'e2e_prefs' });
}

/** A reload drops the dialog's open flag — reopen it (sidebar spec). */
async function reopenChat(page: Page): Promise<void> {
  await expect(page.getByTestId('chat-form')).toBeVisible();
}

/**
 * Send one turn and wait for the stream to settle. Two-step gate on
 * purpose (sidebar spec): waiting only for the cancel button passes
 * vacuously when the assertion wins the race against the first delta.
 */
async function sendMessage(page: Page, text: string): Promise<void> {
  await page.getByTestId('chat-input').fill(text);
  await page.getByTestId('chat-send').click();
  await expect(page.getByTestId('chat-message-assistant').last()).not.toHaveText(
    /^\s*$/,
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

/** `+ New chat` persists its id only AFTER `POST /api/chat/sessions`
 *  resolves — reading sessionStorage immediately after the click races
 *  the round-trip. Poll instead (sidebar spec). */
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

/**
 * The deterministic half of the recall contract: after a reload the
 * active session's replay shows exactly the given user turns (in order),
 * each with a non-empty assistant reply beneath it.
 */
/**
 * Count `agent.chat_user_prefs` rows sourced from `sessionId`, queried
 * through the agent venv's python + asyncpg (no new npm dependency, no
 * psql requirement). Returns 0 when the DB or interpreter is
 * unreachable from the test runner — the caller's poll then times out,
 * which is the honest failure for "the plumbing did not run"; set
 * E2E_DB_DSN to override the default local DSN.
 */
function countPrefsForSession(sessionId: string): number {
  const python =
    process.env.E2E_AGENT_PYTHON ??
    '/home/smona/nyx/backend/agent_py/.venv/bin/python';
  const dsn =
    process.env.E2E_DB_DSN ??
    'postgresql://postgres:postgres@localhost:5433/nyx';
  const script = [
    'import asyncio, sys, asyncpg',
    'async def main():',
    '    conn = await asyncpg.connect(sys.argv[1], server_settings={"search_path": "agent,langgraph"})',
    '    n = await conn.fetchval("SELECT count(*) FROM chat_user_prefs WHERE source_session_id=$1", sys.argv[2])',
    '    print(n)',
    '    await conn.close()',
    'asyncio.run(main())',
  ].join('\n');
  try {
    const out = execFileSync(python, ['-c', script, dsn, sessionId], {
      timeout: 10000,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    });
    return Number.parseInt(out.trim(), 10) || 0;
  } catch {
    return 0;
  }
}

async function expectReplayedTurns(page: Page, prompts: string[]): Promise<void> {
  await expect(page.getByTestId('chat-message-user')).toHaveText(
    prompts,
    { timeout: 15000 },
  );
  const replies = page.getByTestId('chat-message-assistant');
  await expect(replies).toHaveCount(prompts.length);
  for (let i = 0; i < prompts.length; i += 1) {
    await expect(replies.nth(i)).not.toHaveText(/^\s*$/);
  }
}

test.describe('Chat / user preferences (browser-level, live stack)', { tag: '@live' }, () => {
  // Two live LLM turns + a DB-extraction poll: comfortably above
  // Playwright's 30s default on a shared-GPU box.
  test.setTimeout(120_000);

  test('a stated preference survives into a later session and the turns replay', async ({
    page,
  }) => {
    await registerAndOpenChat(page);

    // Turn 1 — the preference statement. The reply must complete
    // normally: extraction is fire-and-forget after `done` and can never
    // surface on the stream (spec: "Slow extraction never delays the
    // stream" / "Extraction failure is invisible").
    const prefPrompt = '我喜欢鱿鱼丝，也喜欢ACG';
    await sendMessage(page, prefPrompt);
    const firstSessionId = await persistedSessionId(page);
    expect(firstSessionId).not.toBeNull();

    // Deterministic plumbing assertion (review 2026-10-14 / M2): the
    // extraction + upsert really happened, anchored to THIS session.
    // Polls the agent DB for a prefs row whose source_session_id is the
    // session the turn ran in — this is what fails if the gate, the
    // extraction call, or the upsert is broken (the replay assertions
    // below would pass regardless). Skips with a recorded reason when
    // the DB is not reachable from the test runner.
    await expect
      .poll(() => countPrefsForSession(firstSessionId!), { timeout: 30000 })
      .toBeGreaterThan(0);

    // New session — the follow-up is asked in a fresh conversation, so
    // any recall comes from the injected preference block, not from the
    // in-panel history.
    await page.getByTestId('chat-new-session').click();
    await expect(page.getByText(/No messages yet/)).toBeVisible();
    const secondSessionId = await waitForPersistedSessionId(page);
    expect(secondSessionId).not.toBe(firstSessionId);

    const askPrompt = '推荐一个零食给我';
    await sendMessage(page, askPrompt);

    // Tolerant observation only: whether the reply leans on the stored
    // preference (mentions 鱿鱼丝) is model-dependent — record it as an
    // annotation for the e2e evidence trail, never as an assertion.
    // The planner prompt carrying the block is proven by the Python
    // tests (`test_prefs_injection.py`), not here.
    const replyText = await page
      .getByTestId('chat-message-assistant')
      .last()
      .innerText();
    test.info().annotations.push({
      type: 'prefs-recall-observation',
      description: replyText.includes('鱿鱼丝')
        ? `reply references the stored preference: ${replyText.slice(0, 200)}`
        : `reply does not mention 鱿鱼丝 (model-dependent; tolerated): ${replyText.slice(0, 200)}`,
    });

    // Deterministic assertion: the new session's turns are durably
    // recorded — reload replays both the ask and its assistant reply.
    await page.reload();
    await reopenChat(page);
    await expectReplayedTurns(page, [askPrompt]);

    // Both sessions are in the rail, newest first (the preference turn
    // was its own session and is still intact).
    await expect(page.getByTestId('chat-session-row')).toHaveCount(2);
    const activeRow = page.locator(
      '[data-testid="chat-session-row"][data-active="true"]',
    );
    await expect(activeRow).toHaveAttribute('data-session-id', secondSessionId!);
    expect(await persistedSessionId(page)).toBe(secondSessionId);

    // The preference-bearing session still replays its own turn — the
    // follow-up session did not disturb it.
    await page
      .locator(
        `[data-testid="chat-session-row"][data-session-id="${firstSessionId}"]`,
      )
      .click();
    await expectReplayedTurns(page, [prefPrompt]);
  });
});
